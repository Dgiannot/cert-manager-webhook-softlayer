package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	certmanagerv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/jetstack/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/acme/webhook/cmd"

	"github.com/softlayer/softlayer-go/datatypes"
	"github.com/softlayer/softlayer-go/filter"
	"github.com/softlayer/softlayer-go/services"
	"github.com/softlayer/softlayer-go/session"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var GroupName = os.Getenv("GROUP_NAME")

func main() {
	if GroupName == "" {
		panic("GROUP_NAME must be specified")
	}

	// This will register our softlayer DNS provider with the webhook serving
	// library, making it available as an API under the provided GroupName.
	// You can register multiple DNS provider implementations with a single
	// webhook, where the Name() method will be used to disambiguate between
	// the different implementations.
	cmd.RunWebhookServer(GroupName,
		&softlayerDNSProviderSolver{},
	)
}

// softlayerDNSProviderSolver implements the provider-specific logic needed to
// 'present' an ACME challenge TXT record for your own DNS provider.
// To do so, it must implement the `github.com/jetstack/cert-manager/pkg/acme/webhook.Solver`
// interface.
type softlayerDNSProviderSolver struct {
	client *kubernetes.Clientset
}

// softlayerDNSProviderConfig is a structure that is used to decode into when
// solving a DNS01 challenge.
// This information is provided by cert-manager, and may be a reference to
// additional configuration that's needed to solve the challenge for this
// particular certificate or issuer.
// This typically includes references to Secret resources containing DNS
// provider credentials, in cases where a 'multi-tenant' DNS solver is being
// created.
// If you do *not* require per-issuer or per-certificate configuration to be
// provided to your webhook, you can skip decoding altogether in favour of
// using CLI flags or similar to provide configuration.
// You should not include sensitive information here. If credentials need to
// be used by your provider here, you should reference a Kubernetes Secret
// resource and fetch these credentials using a Kubernetes clientset.
type softlayerDNSProviderConfig struct {
	// Change the two fields below according to the format of the configuration
	// to be decoded.
	// These fields will be set by users in the
	// `issuer.spec.acme.dns01.providers.webhook.config` field.

	Username        string                          `json:"username"`
	APIKeySecretRef certmanagerv1.SecretKeySelector `json:"apiKeySecretRef"`
}

// Name is used as the name for this DNS solver when referencing it on the ACME
// Issuer resource.
// This should be unique **within the group name**, i.e. you can have two
// solvers configured with the same Name() **so long as they do not co-exist
// within a single webhook deployment**.
// For example, `cloudflare` may be used as the name of a solver.
func (c *softlayerDNSProviderSolver) Name() string {
	return "softlayer-solver"
}

func (c *softlayerDNSProviderSolver) provider(cfg *softlayerDNSProviderConfig, namespace string) (*session.Session, error) {
	sec, err := c.client.CoreV1().Secrets(namespace).Get(cfg.APIKeySecretRef.LocalObjectReference.Name, metav1.GetOptions{})
	klog.Infof("config: %v", cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to get secret: %v", err)
	}

	secBytes, ok := sec.Data[cfg.APIKeySecretRef.Key]
	if !ok {
		return nil, fmt.Errorf("Key %q not found in secret \"%s/%s\"", cfg.APIKeySecretRef.Key, cfg.APIKeySecretRef.LocalObjectReference.Name, namespace)
	}

	apiKey := string(secBytes)

	return session.New(cfg.Username, apiKey), nil
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *softlayerDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	klog.Infof("call present: namespace=%s, zone=%s", ch.ResourceNamespace, ch.ResolvedZone)
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return fmt.Errorf("unable to load config: %s", err)
	}

	provider, err := c.provider(&cfg, ch.ResourceNamespace)
	if err != nil {
		return fmt.Errorf("unable to get provider: %s", err)
	}

	zone, err := c.getHostedZone(provider, ch.ResolvedZone)
	if err != nil {
		return fmt.Errorf("unable to get hosted zone: %s", err)
	}

	// Look for existing records.
	svc := services.GetDnsDomainService(provider)
	records, err := svc.Id(*zone).GetResourceRecords()
	if len(records) == 0 || err != nil {
		return fmt.Errorf("unable to get resource records: %s", err)
	}

	entry := strings.TrimSuffix(ch.ResolvedFQDN, "."+ch.ResolvedZone)

	recordsTxt, err := c.findTxtRecords(provider, *zone, entry, ch.Key)
	if err != nil {
		return fmt.Errorf("unable to find txt records: %s", err)
	}

	if len(recordsTxt) > 0 {
		// the record is already set to the desired value
		return nil
	}

	if len(recordsTxt) >= 1 {
		svcRecord := services.GetDnsDomainResourceRecordService(provider)
		del, err := svcRecord.DeleteObjects(recordsTxt)
		if del == false || err != nil {
			return fmt.Errorf("unable to delete objects: %s", err)
		}
	}

	ttl := 60
	_, err = svc.Id(*zone).CreateTxtRecord(&entry, &ch.Key, &ttl)
	if err != nil {
		return fmt.Errorf("unable to create txt record: %s", err)
	}

	return nil
}

// CleanUp should delete the relevant TXT record from the DNS provider console.
// If multiple TXT records exist with the same record name (e.g.
// _acme-challenge.example.com) then **only** the record with the same `key`
// value provided on the ChallengeRequest should be cleaned up.
// This is in order to facilitate multiple DNS validations for the same domain
// concurrently.
func (c *softlayerDNSProviderSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	provider, err := c.provider(&cfg, ch.ResourceNamespace)
	if err != nil {
		return err
	}

	zone, err := c.getHostedZone(provider, ch.ResolvedZone)
	if err != nil {
		return err
	}

	entry := strings.TrimSuffix(ch.ResolvedFQDN, "."+ch.ResolvedZone)
	records, err := c.findTxtRecords(provider, *zone, entry, ch.Key)
	if err != nil {
		return err
	}

	svc := services.GetDnsDomainResourceRecordService(provider)
	del, err := svc.DeleteObjects(records)
	if del == false || err != nil {
		return err
	}
	return nil
}

// Initialize will be called when the webhook first starts.
// This method can be used to instantiate the webhook, i.e. initialising
// connections or warming up caches.
// Typically, the kubeClientConfig parameter is used to build a Kubernetes
// client that can be used to fetch resources from the Kubernetes API, e.g.
// Secret resources containing credentials used to authenticate with DNS
// provider accounts.
// The stopCh can be used to handle early termination of the webhook, in cases
// where a SIGTERM or similar signal is sent to the webhook process.
func (c *softlayerDNSProviderSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	klog.Info("Initialize softlayer solver")
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return fmt.Errorf("unable to get k8s client: %s", err)
	}

	c.client = cl
	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extapi.JSON) (softlayerDNSProviderConfig, error) {
	klog.Infof("raw config: %v", cfgJSON.String())
	cfg := softlayerDNSProviderConfig{}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}
	return cfg, nil
}

// getHostedZone returns the managed-zone
func (c *softlayerDNSProviderSolver) getHostedZone(session *session.Session, domain string) (*int, error) {
	svc := services.GetAccountService(session)

	filters := filter.New(
		filter.Path("domains.name").Eq(strings.TrimSuffix(domain, ".")),
	)

	zones, err := svc.Filter(filters.Build()).GetDomains()

	if err != nil {
		return nil, fmt.Errorf("Softlayer API call failed: %v", err)
	}

	if len(zones) == 0 {
		return nil, fmt.Errorf("No matching Softlayer domain found for domain %s", domain)
	}

	if len(zones) > 1 {
		return nil, fmt.Errorf("Too many Softlayer domains found for domain %s", domain)
	}

	return zones[0].Id, nil
}

func (c *softlayerDNSProviderSolver) findTxtRecords(session *session.Session, zone int, entry, key string) ([]datatypes.Dns_Domain_ResourceRecord, error) {
	txtType := "txt"
	// Look for existing records.
	svc := services.GetDnsDomainService(session)

	filters := filter.New(
		filter.Path("resourceRecords.type").Eq(txtType),
		filter.Path("resourceRecords.host").Eq(entry),
	)

	recs, err := svc.Id(zone).Filter(filters.Build()).GetResourceRecords()
	if err != nil {
		return nil, err
	}

	found := []datatypes.Dns_Domain_ResourceRecord{}
	for _, r := range recs {
		if *r.Type == txtType && *r.Host == entry && *r.Data == key {
			found = append(found, r)
		}
	}

	return found, nil
}
