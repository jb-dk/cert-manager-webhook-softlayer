package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
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

	cis "github.com/IBM-Cloud/bluemix-go/api/cis/cisv1"
	cissession "github.com/IBM-Cloud/bluemix-go/session"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var GroupName = os.Getenv("GROUP_NAME")

func main() {
	log.Infof("main with GROUP_NAME %s", GroupName)
	if GroupName == "" {
		panic("GROUP_NAME must be specified")
	}

	if os.Getenv("IC_API_KEY") == "" {
		log.Fatal("failed to initialize ibmdns provider. Please set IC_API_KEY to a valid API key")
		panic("IC_API_KEY must be specified")
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
	client    *kubernetes.Clientset
	ibmCisAPI cis.CisServiceAPI
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

	CisCRN          string                   `json:"cisCRN"`
	APIKeySecretRef cmmeta.SecretKeySelector `json:"apiKeySecretRef"`
}

// Name is used as the name for this DNS solver when referencing it on the ACME
// Issuer resource.
// This should be unique **within the group name**, i.e. you can have two
// solvers configured with the same Name() **so long as they do not co-exist
// within a single webhook deployment**.
// For example, `cloudflare` may be used as the name of a solver.
func (c *softlayerDNSProviderSolver) Name() string {
	log.Info("Name() called")
	return "softlayer"
}

func (c *softlayerDNSProviderSolver) validate(cfg *softlayerDNSProviderConfig) error {
	// Check that the username is defined
	log.Infof("validate(%s)", cfg)

	if cfg.CisCRN == "" {
		return fmt.Errorf("No IBM Cloud Internet Service CRN provided")
	}

	// Try to load the API secret name
	if cfg.APIKeySecretRef.LocalObjectReference.Name == "" {
		return fmt.Errorf("No API key to access IBM Cloud Internet Service provided")
	}

	// Try to load the API secret key
	if cfg.APIKeySecretRef.Key == "" {
		return fmt.Errorf("No API key to access IBM Cloud Internet Service provided")
	}
	return nil
}

func (c *softlayerDNSProviderSolver) provider(cfg *softlayerDNSProviderConfig, namespace string) (*session.Session, error) {
	log.Infof("provider(namespace='%s' )", namespace)
	secretName := cfg.APIKeySecretRef.LocalObjectReference.Name
	log.Infof("Try to load secret `%s` with key `%s`", secretName, cfg.APIKeySecretRef.Key)
	klog.V(6).Infof("Try to load secret `%s` with key `%s`", secretName, cfg.APIKeySecretRef.Key)
	sec, err := c.client.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("unable to get secret `%s`; %v", secretName, err)
		return nil, fmt.Errorf("unable to get secret `%s`; %v", secretName, err)
	}

	secBytes, ok := sec.Data[cfg.APIKeySecretRef.Key]
	if !ok {
		log.Errorf("Key %q not found in secret \"%s/%s\"", cfg.APIKeySecretRef.Key, cfg.APIKeySecretRef.LocalObjectReference.Name, namespace)
		return nil, fmt.Errorf("Key %q not found in secret \"%s/%s\"", cfg.APIKeySecretRef.Key, cfg.APIKeySecretRef.LocalObjectReference.Name, namespace)
	}

	apiKey := string(secBytes)

	return session.New(cfg.CisCRN, apiKey), nil
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *softlayerDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	// log.Infof("Present(ch='%s' )", ch)
	// log.Infof("call function Present: config %s", ch.Config)

	log.Infof("Present(namespace=%s, zone=%s, fqdn=%s key=%s)", ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN, ch.Key)

	klog.V(6).Infof("call function Present: namespace=%s, zone=%s, fqdn=%s", ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN)
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		log.Errorf("unable to load config: %s", err)
		return fmt.Errorf("unable to load config: %s", err)
	}
	//	log.Infof("call function Present: config '%s'", cfg)

	zonesAPI := c.ibmCisAPI.Zones()

	// Lets loop through the CRNs listed, if more than one is provided they should be comma-separated (,)
	// Todo change it to an array in the Issuer CRD, rather than comma seperated
	for i, crn := range strings.Split(cfg.CisCRN, ",") {
		log.Debugf("CRN %d - %s", i, crn)

		myZones, ibmErr := zonesAPI.ListZones(crn)

		if ibmErr != nil {
			log.Fatal(ibmErr)
		}

		for _, zoneid := range myZones {
			log.Debugf("Zone name '%s' id %s", zoneid.Name, zoneid.Id)

			// Ensure to add completing . only if it idoes not exist! - todo
			if strings.HasSuffix(ch.ResolvedFQDN, zoneid.Name+".") {
				log.Debugf("Zone %s is a match", zoneid.Name)

				// Check if TXT record exist?

				// create TX record with challenge
				dnsAPI := c.ibmCisAPI.Dns()

				// Todo check if specific TTLs should be used?

				_, ibmErr := dnsAPI.CreateDns(crn, zoneid.Id, cis.DnsBody{
					Name:    ch.ResolvedFQDN,
					DnsType: "TXT",
					Content: ch.Key,
				})

				if ibmErr != nil {
					log.Error(ibmErr)
				}
				log.Infof("Present DNS01 challenge is now created %s = %s", ch.ResolvedFQDN, ch.Key)

			}
		}
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

	log.Infof("CleanUp(namespace=%s, zone=%s, fqdn=%s, key=%s", ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN, ch.Key)
	klog.V(6).Infof("call function CleanUp: namespace=%s, zone=%s, fqdn=%s", ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN)
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	zonesAPI := c.ibmCisAPI.Zones()

	// Lets loop through the CRNs listed, if more than one is provided they should be comma-separated (,)
	// Todo change it to an array in the Issuer CRD, rather than comma seperated
	for i, crn := range strings.Split(cfg.CisCRN, ",") {
		log.Debugf("CRN %d - %s", i, crn)

		myZones, ibmErr := zonesAPI.ListZones(crn)

		if ibmErr != nil {
			log.Fatal(ibmErr)
		}

		for _, zoneid := range myZones {
			log.Debugf("Zone name '%s' id %s", zoneid.Name, zoneid.Id)

			// Ensure to add completing . only if it idoes not exist! - todo
			if strings.HasSuffix(ch.ResolvedFQDN, zoneid.Name+".") {
				log.Debugf("Zone %s is a match", zoneid.Name)

				dnsAPI := c.ibmCisAPI.Dns()

				myDnsrecs, ibmErr := dnsAPI.ListDns(crn, zoneid.Id)
				if ibmErr != nil {
					log.Fatal(ibmErr)
				}

				// delete the specific TX record with challenge
				// Only the correct TXT record, with the correct name
				// and with the correct content will be deleted
				for _, myDnsrec := range myDnsrecs {
					if myDnsrec.DnsType != "TXT" {
						log.Debugf(" Skipping non TXT record: %s", myDnsrec)
						continue
					}
					if (myDnsrec.Name + ".") != ch.ResolvedFQDN {
						log.Debugf(" Skipping TXT record with different name: %s", myDnsrec)
						continue
					}
					if myDnsrec.Content != ch.Key {
						log.Debugf(" Skipping TXT record with different content: %s", myDnsrec)
						continue
					}
					log.Infof("Found record to remove as challenge, will request to delete it now. Rec: %s", myDnsrec)
					dnsAPI.DeleteDns(crn, zoneid.Id, myDnsrec.Id)
				}
			}
		}
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
	log.Infof("Starting up webhook service")
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		log.Errorf("unable to get k8s client: %s", err)
		return fmt.Errorf("unable to get k8s client: %s", err)
	}

	log.Debugf("Kubernetes client in place")

	ibmSession, ibmErr := cissession.New()

	if ibmErr != nil {
		log.Fatalf("IBM Cloud session failed: %s", ibmErr)
	}

	ibmCisAPI, ibmErr := cis.New(ibmSession)
	if ibmErr != nil {
		log.Fatal(ibmErr)
	}
	log.Debugf("IBM Cloud Internet Services API instance %s", ibmCisAPI)

	c.ibmCisAPI = ibmCisAPI
	c.client = cl
	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extapi.JSON) (softlayerDNSProviderConfig, error) {
	log.Debug("loadConfig()")

	cfg := softlayerDNSProviderConfig{}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		log.Errorf("error decoding solver config: %v", err)
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}
	log.Debugf("loadConfig(%s)", cfg)

	return cfg, nil
}

// getHostedZone returns the managed-zone
func (c *softlayerDNSProviderSolver) getHostedZone(session *session.Session, domain string) (*int, error) {
	log.Infof("getHostedZone(%s)", domain)
	svc := services.GetAccountService(session)

	filters := filter.New(
		filter.Path("domains.name").Eq(strings.TrimSuffix(domain, ".")),
	)

	zones, err := svc.Filter(filters.Build()).GetDomains()

	if err != nil {
		log.Errorf("Softlayer API call failed: %v", err)
		return nil, fmt.Errorf("Softlayer API call failed: %v", err)
	}

	if len(zones) == 0 {
		log.Errorf("No matching Softlayer domain found for domain %s", domain)
		return nil, fmt.Errorf("No matching Softlayer domain found for domain %s", domain)
	}

	if len(zones) > 1 {
		log.Errorf("Too many Softlayer domains found for domain %s", domain)
		return nil, fmt.Errorf("Too many Softlayer domains found for domain %s", domain)
	}

	return zones[0].Id, nil
}

func (c *softlayerDNSProviderSolver) findTxtRecords(session *session.Session, zone int, entry, key string) ([]datatypes.Dns_Domain_ResourceRecord, error) {
	log.Infof("findTxtRecords(zone=%d, entry=%s, key=%s)", zone, entry, key)

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
