package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/privatedns/mgmt/2018-09-01/privatedns"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/platform/config/env"
)

// dnsProviderPrivate implements the challenge.Provider interface for Azure Private Zone DNS.
type dnsProviderPrivate struct {
	config     *Config
	authorizer autorest.Authorizer
}

// Timeout returns the timeout and interval to use when checking for DNS propagation.
// Adjusting here to cope with spikes in propagation times.
func (d *dnsProviderPrivate) Timeout() (timeout, interval time.Duration) {
	return d.config.PropagationTimeout, d.config.PollingInterval
}

// Present creates a TXT record to fulfill the dns-01 challenge.
func (d *dnsProviderPrivate) Present(domain, token, keyAuth string) error {
	ctx := context.Background()
	info := dns01.GetChallengeInfo(domain, keyAuth)

	zone, err := d.getHostedZoneID(ctx, info.EffectiveFQDN)
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}

	rsc := privatedns.NewRecordSetsClientWithBaseURI(d.config.ResourceManagerEndpoint, d.config.SubscriptionID)
	rsc.Authorizer = d.authorizer

	subDomain, err := dns01.ExtractSubDomain(info.EffectiveFQDN, zone)
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}

	// Get existing record set
	rset, err := rsc.Get(ctx, d.config.ResourceGroup, zone, privatedns.TXT, subDomain)
	if err != nil {
		var detailed autorest.DetailedError
		if !errors.As(err, &detailed) || detailed.StatusCode != http.StatusNotFound {
			return fmt.Errorf("azure: %w", err)
		}
	}

	// Construct unique TXT records using map
	uniqRecords := map[string]struct{}{info.Value: {}}
	if rset.RecordSetProperties != nil && rset.TxtRecords != nil {
		for _, txtRecord := range *rset.TxtRecords {
			// Assume Value doesn't contain multiple strings
			values := to.StringSlice(txtRecord.Value)
			if len(values) > 0 {
				uniqRecords[values[0]] = struct{}{}
			}
		}
	}

	var txtRecords []privatedns.TxtRecord
	for txt := range uniqRecords {
		txtRecords = append(txtRecords, privatedns.TxtRecord{Value: &[]string{txt}})
	}

	rec := privatedns.RecordSet{
		Name: &subDomain,
		RecordSetProperties: &privatedns.RecordSetProperties{
			TTL:        to.Int64Ptr(int64(d.config.TTL)),
			TxtRecords: &txtRecords,
		},
	}

	_, err = rsc.CreateOrUpdate(ctx, d.config.ResourceGroup, zone, privatedns.TXT, subDomain, rec, "", "")
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}
	return nil
}

// CleanUp removes the TXT record matching the specified parameters.
func (d *dnsProviderPrivate) CleanUp(domain, token, keyAuth string) error {
	ctx := context.Background()
	info := dns01.GetChallengeInfo(domain, keyAuth)

	zone, err := d.getHostedZoneID(ctx, info.EffectiveFQDN)
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}

	subDomain, err := dns01.ExtractSubDomain(info.EffectiveFQDN, zone)
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}

	rsc := privatedns.NewRecordSetsClientWithBaseURI(d.config.ResourceManagerEndpoint, d.config.SubscriptionID)
	rsc.Authorizer = d.authorizer

	_, err = rsc.Delete(ctx, d.config.ResourceGroup, zone, privatedns.TXT, subDomain, "")
	if err != nil {
		return fmt.Errorf("azure: %w", err)
	}
	return nil
}

// Checks that azure has a zone for this domain name.
func (d *dnsProviderPrivate) getHostedZoneID(ctx context.Context, fqdn string) (string, error) {
	if zone := env.GetOrFile(EnvZoneName); zone != "" {
		return zone, nil
	}

	authZone, err := dns01.FindZoneByFqdn(fqdn)
	if err != nil {
		return "", err
	}

	dc := privatedns.NewPrivateZonesClientWithBaseURI(d.config.ResourceManagerEndpoint, d.config.SubscriptionID)
	dc.Authorizer = d.authorizer

	zone, err := dc.Get(ctx, d.config.ResourceGroup, dns01.UnFqdn(authZone))
	if err != nil {
		return "", err
	}

	// zone.Name shouldn't have a trailing dot(.)
	return to.String(zone.Name), nil
}
