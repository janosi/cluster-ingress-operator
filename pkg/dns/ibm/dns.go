package ibm

import (
	"fmt"
	"net/http"
	"time"

	"github.com/IBM/go-sdk-core/v5/core"
	"github.com/IBM/networking-go-sdk/dnsrecordsv1"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/dns"

	iov1 "github.com/openshift/api/operatoringress/v1"
	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

var (
	_   dns.Provider = &Provider{}
	log              = logf.Logger.WithName("dns")
)

type Provider struct {
	dnsServices map[string]*dnsrecordsv1.DnsRecordsV1
}

// Config is the necessary input to configure the manager.
type Config struct {
	APIKey    string
	CISCRN    string
	UserAgent string
	Zones     []string
}

func NewProvider(config Config) (*Provider, error) {
	if len(config.Zones) < 1 {
		return nil, fmt.Errorf("missing zone data")
	}
	authenticator := &core.IamAuthenticator{
		ApiKey: config.APIKey,
	}
	provider := &Provider{}

	for _, zone := range config.Zones {
		options := &dnsrecordsv1.DnsRecordsV1Options{
			Authenticator:  authenticator,
			URL:            "https://api.private.cis.cloud.ibm.com/v1",
			Crn:            &config.CISCRN,
			ZoneIdentifier: &zone,
		}

		dnsService, err := dnsrecordsv1.NewDnsRecordsV1(options)
		if err != nil {
			return nil, fmt.Errorf("failed to create a new DNS Service instance: %v", err)
		}
		dnsService.EnableRetries(3, 5*time.Second)
		dnsService.Service.SetUserAgent(config.UserAgent)

		provider.dnsServices[zone] = dnsService
	}

	if err := validateDNSServices(provider); err != nil {
		return nil, fmt.Errorf("failed to validate ibm dns services: %v", err)
	}
	return provider, nil
}

// validateDNSServices validates that provider clients can communicate with
// associated API endpoints by having each client make a get DNS records call.
func validateDNSServices(provider *Provider) error {
	var errs []error
	maxItems := int64(1)
	for _, dnsService := range provider.dnsServices {
		opt := dnsService.NewListAllDnsRecordsOptions()
		opt.PerPage = &maxItems
		if _, _, err := dnsService.ListAllDnsRecords(opt); err != nil {
			errs = append(errs, fmt.Errorf("failed to get dns records: %v", err))
		}
	}
	return kerrors.NewAggregate(errs)
}

func (p *Provider) Ensure(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	return p.createOrUpdateDNSRecord(record, zone)
}

func (p *Provider) Replace(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	return p.createOrUpdateDNSRecord(record, zone)
}

func (p *Provider) Delete(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	if err := validateInputDNSData(record, zone); err != nil {
		return fmt.Errorf("delete: invalid dns input data: %v", err)
	}
	dnsService := p.dnsServices[zone.ID]
	opt := dnsService.NewListAllDnsRecordsOptions()
	opt.SetType(string(record.Spec.RecordType))
	opt.SetName(record.Spec.DNSName)
	for _, target := range record.Spec.Targets {
		opt.SetContent(target)
		result, response, err := dnsService.ListAllDnsRecords(opt)
		if err != nil {
			if response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("delete: failed to list the dns record: %v", err)
			}
			continue
		}
		if result != nil && result.Result != nil {
			for _, resultData := range result.Result {
				if resultData.ID != nil {
					delOpt := dnsService.NewDeleteDnsRecordOptions(*resultData.ID)
					_, delResponse, err := dnsService.DeleteDnsRecord(delOpt)
					if err != nil && delResponse.StatusCode != http.StatusNotFound {
						return fmt.Errorf("delete: failed to delete the dns record: %v", err)
					}
				} else {
					return fmt.Errorf("delete: record id is nil")
				}
			}
		} else {
			return fmt.Errorf("delete: invalid result")
		}
	}
	return nil
}

func (p *Provider) createOrUpdateDNSRecord(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	if err := validateInputDNSData(record, zone); err != nil {
		return fmt.Errorf("createOrUpdateDNSRecord: invalid dns input data: %v", err)
	}
	dnsService := p.dnsServices[zone.ID]
	listOpt := dnsService.NewListAllDnsRecordsOptions()
	listOpt.SetType(string(record.Spec.RecordType))
	listOpt.SetName(record.Spec.DNSName)
	for _, target := range record.Spec.Targets {
		listOpt.SetContent(target)
		result, _, err := dnsService.ListAllDnsRecords(listOpt)
		if err != nil {
			return fmt.Errorf("createOrUpdateDNSRecord: failed to list the dns record: %v", err)
		}
		if result != nil && result.Result != nil {
			if len(result.Result) == 0 {
				createOpt := dnsService.NewCreateDnsRecordOptions()
				createOpt.SetName(record.Spec.DNSName)
				createOpt.SetType(string(record.Spec.RecordType))
				createOpt.SetContent(target)
				_, _, err := dnsService.CreateDnsRecord(createOpt)
				if err != nil {
					return fmt.Errorf("createOrUpdateDNSRecord: failed to create the dns record: %v", err)
				}
			} else {
				updateOpt := dnsService.NewUpdateDnsRecordOptions(*result.Result[0].ID)
				updateOpt.SetName(record.Spec.DNSName)
				updateOpt.SetType(string(record.Spec.RecordType))
				updateOpt.SetContent(record.Spec.Targets[0])
				_, _, err := dnsService.UpdateDnsRecord(updateOpt)
				if err != nil {
					return fmt.Errorf("createOrUpdateDNSRecord: failed to update the dns record: %v", err)
				}
			}
		} else {
			return fmt.Errorf("createOrUpdateDNSRecord: invalid result")
		}
	}

	return nil
}

func validateInputDNSData(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	if record == nil {
		return fmt.Errorf("validateInputDNSData: dns record is nil")
	}
	if record.Spec.DNSName == "" {
		return fmt.Errorf("validateInputDNSData: dns record name is empty")
	}
	if record.Spec.RecordType == "" {
		return fmt.Errorf("validateInputDNSData: dns record type is empty")
	}
	if len(record.Spec.Targets) == 0 {
		return fmt.Errorf("validateInputDNSData: dns record content is empty")
	}
	if zone.ID == "" {
		return fmt.Errorf("validateInputDNSData: dns zone id is empty")
	}
	return nil
}
