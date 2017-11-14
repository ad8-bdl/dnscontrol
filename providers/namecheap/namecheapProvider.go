package namecheap

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/StackExchange/dnscontrol/models"
	"github.com/StackExchange/dnscontrol/providers"
	"github.com/StackExchange/dnscontrol/providers/diff"
	nc "github.com/billputer/go-namecheap"
	"github.com/miekg/dns/dnsutil"
)

var NamecheapDefaultNs = []string{"dns1.registrar-servers.com", "dns2.registrar-servers.com"}

type Namecheap struct {
	ApiKey  string
	ApiUser string
	client  *nc.Client
}

var docNotes = providers.DocumentationNotes{
	providers.DocCreateDomains:       providers.Cannot("Requires domain registered through their service"),
	providers.DocOfficiallySupported: providers.Cannot(),
	providers.DocDualHost:            providers.Cannot("Doesn't allow control of apex NS records"),
	providers.CanUseAlias:            providers.Cannot(),
	providers.CanUseCAA:              providers.Cannot(),
	providers.CanUseSRV:              providers.Cannot("The namecheap web console allows you to make SRV records, but their api does not let you read or set them"),
	providers.CanUsePTR:              providers.Cannot(),
	providers.CanUseTLSA:             providers.Cannot(),
}

func init() {
	providers.RegisterRegistrarType("NAMECHEAP", newReg)
	providers.RegisterDomainServiceProviderType("NAMECHEAP", newDsp, providers.CantUseNOPURGE, docNotes)
	providers.RegisterCustomRecordType("URL", "NAMECHEAP", "")
	providers.RegisterCustomRecordType("URL301", "NAMECHEAP", "")
	providers.RegisterCustomRecordType("FRAME", "NAMECHEAP", "")
}

func newDsp(conf map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	return newProvider(conf, metadata)
}

func newReg(conf map[string]string) (providers.Registrar, error) {
	return newProvider(conf, nil)
}

func newProvider(m map[string]string, metadata json.RawMessage) (*Namecheap, error) {
	api := &Namecheap{}
	api.ApiUser, api.ApiKey = m["apiuser"], m["apikey"]
	if api.ApiKey == "" || api.ApiUser == "" {
		return nil, fmt.Errorf("Namecheap apikey and apiuser must be provided.")
	}
	api.client = nc.NewClient(api.ApiUser, api.ApiKey, api.ApiUser)
	// if BaseURL is specified in creds, use that url
	BaseURL, ok := m["BaseURL"]
	if ok {
		api.client.BaseURL = BaseURL
	}
	return api, nil
}

func splitDomain(domain string) (sld string, tld string) {
	tld, _ = publicsuffix.PublicSuffix(domain)
	d, _ := publicsuffix.EffectiveTLDPlusOne(domain)
	sld = strings.Split(d, ".")[0]
	return sld, tld
}

// namecheap has request limiting at unpublished limits
// from support in SEP-2017:
//    "The limits for the API calls will be 20/Min, 700/Hour and 8000/Day for one user.
//     If you can limit the requests within these it should be fine."
// this helper performs some api action, checks for rate limited response, and if so, enters a retry loop until it resolves
// if you are consistently hitting this, you may have success asking their support to increase your account's limits.
func doWithRetry(f func() error) {
	// sleep 5 seconds at a time, up to 23 times (1 minute, 15 seconds)
	const maxRetries = 23
	const sleepTime = 5 * time.Second
	var currentRetry int
	for {
		err := f()
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "Error 500000: Too many requests") {
			currentRetry++
			if currentRetry >= maxRetries {
				return
			}
			log.Printf("Namecheap rate limit exceeded. Waiting %s to retry.", sleepTime)
			time.Sleep(sleepTime)
		} else {
			return
		}
	}
}

func (n *Namecheap) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	dc.Punycode()
	sld, tld := splitDomain(dc.Name)
	var records *nc.DomainDNSGetHostsResult
	var err error
	doWithRetry(func() error {
		records, err = n.client.DomainsDNSGetHosts(sld, tld)
		return err
	})
	if err != nil {
		return nil, err
	}

	var actual []*models.RecordConfig

	// namecheap does not allow setting @ NS with basic DNS
	dc.Filter(func(r *models.RecordConfig) bool {
		if r.Type == "NS" && r.Name == "@" {
			if !strings.HasSuffix(r.Target, "registrar-servers.com.") {
				fmt.Println("\n", r.Target, "Namecheap does not support changing apex NS records. Skipping.")
			}
			return false
		}
		return true
	})

	// namecheap has this really annoying feature where they add some parking records if you have no records.
	// This causes a few problems for our purposes, specifically the integration tests.
	// lets detect that one case and pretend it is a no-op.
	if len(dc.Records) == 0 && len(records.Hosts) == 2 {
		if records.Hosts[0].Type == "CNAME" &&
			strings.Contains(records.Hosts[0].Address, "parkingpage") &&
			records.Hosts[1].Type == "URL" {
			return nil, nil
		}
	}

	for _, r := range records.Hosts {
		if r.Type == "SOA" {
			continue
		}
		rec := &models.RecordConfig{
			NameFQDN:     dnsutil.AddOrigin(r.Name, dc.Name),
			Type:         r.Type,
			Target:       r.Address,
			TTL:          uint32(r.TTL),
			MxPreference: uint16(r.MXPref),
			Original:     r,
		}
		actual = append(actual, rec)
	}

	// Normalize
	models.Downcase(actual)

	differ := diff.New(dc)
	_, create, delete, modify := differ.IncrementalDiff(actual)

	// // because namecheap doesn't have selective create, delete, modify,
	// // we bundle them all up to send at once.  We *do* want to see the
	// // changes though

	var desc []string
	for _, i := range create {
		desc = append(desc, "\n"+i.String())
	}
	for _, i := range delete {
		desc = append(desc, "\n"+i.String())
	}
	for _, i := range modify {
		desc = append(desc, "\n"+i.String())
	}

	msg := fmt.Sprintf("GENERATE_ZONE: %s (%d records)%s", dc.Name, len(dc.Records), desc)
	corrections := []*models.Correction{}

	// only create corrections if there are changes
	if len(desc) > 0 {
		corrections = append(corrections,
			&models.Correction{
				Msg: msg,
				F: func() error {
					return n.generateRecords(dc)
				},
			})
	}

	return corrections, nil
}

func (n *Namecheap) generateRecords(dc *models.DomainConfig) error {

	var recs []nc.DomainDNSHost

	id := 1
	for _, r := range dc.Records {
		name := dnsutil.TrimDomainName(r.NameFQDN, dc.Name)
		rec := nc.DomainDNSHost{
			ID:      id,
			Name:    name,
			Type:    r.Type,
			Address: r.Target,
			MXPref:  int(r.MxPreference),
			TTL:     int(r.TTL),
		}
		recs = append(recs, rec)
		id++
	}
	sld, tld := splitDomain(dc.Name)
	var err error
	doWithRetry(func() error {
		_, err = n.client.DomainDNSSetHosts(sld, tld, recs)
		return err
	})
	return err
}

func (n *Namecheap) GetNameservers(domainName string) ([]*models.Nameserver, error) {
	// return default namecheap nameservers
	ns := NamecheapDefaultNs

	return models.StringsToNameservers(ns), nil
}

func (n *Namecheap) GetRegistrarCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	var info *nc.DomainInfo
	var err error
	doWithRetry(func() error {
		info, err = n.client.DomainGetInfo(dc.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(info.DNSDetails.Nameservers)
	found := strings.Join(info.DNSDetails.Nameservers, ",")
	desiredNs := []string{}
	for _, d := range dc.Nameservers {
		desiredNs = append(desiredNs, d.Name)
	}
	sort.Strings(desiredNs)
	desired := strings.Join(desiredNs, ",")
	if found != desired {
		parts := strings.SplitN(dc.Name, ".", 2)
		sld, tld := parts[0], parts[1]
		return []*models.Correction{
			{
				Msg: fmt.Sprintf("Change Nameservers from '%s' to '%s'", found, desired),
				F: func() (err error) {
					doWithRetry(func() error {
						_, err = n.client.DomainDNSSetCustom(sld, tld, desired)
						return err
					})
					return
				}},
		}, nil
	}
	return nil, nil
}