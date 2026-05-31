package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
)

// Provider names. The set is closed so the UI can render a fixed picker and the
// daily job can iterate a known list. These are the VPS providers with a usable
// billing/account API per the issue.
const (
	ProviderHetzner      = "hetzner"
	ProviderDigitalOcean = "digitalocean"
	ProviderVultr        = "vultr"
	ProviderLinode       = "linode"
)

// ProviderNames returns the supported providers in a stable order.
func ProviderNames() []string {
	return []string{ProviderDigitalOcean, ProviderHetzner, ProviderLinode, ProviderVultr}
}

// SupportedProvider reports whether name is a provider we can fetch billing for.
func SupportedProvider(name string) bool {
	switch name {
	case ProviderHetzner, ProviderDigitalOcean, ProviderVultr, ProviderLinode:
		return true
	}
	return false
}

// Provider knows how to ask one VPS provider for the current month-to-date
// spend and how to read the answer. Request builds the authenticated HTTP
// request; Parse turns a 2xx body into a normalized amount in cents plus an
// ISO-4217 currency. Splitting request-building from parsing lets tests drive
// Parse directly from recorded fixtures without a live network.
type Provider interface {
	Name() string
	Request(ctx context.Context, apiKey string) (*http.Request, error)
	Parse(body []byte) (amountCents int64, currency string, err error)
}

// providerByName returns the Provider implementation for name, or nil.
func providerByName(name string) Provider {
	switch name {
	case ProviderHetzner:
		return hetzner{}
	case ProviderDigitalOcean:
		return digitalOcean{}
	case ProviderVultr:
		return vultr{}
	case ProviderLinode:
		return linode{}
	default:
		return nil
	}
}

// toCents converts a decimal currency amount to integer cents, rounding to the
// nearest cent. Negative inputs (e.g. an account credit shown as a negative
// balance) are clamped to zero — a credit is not a cost.
func toCents(amount float64) int64 {
	if amount < 0 {
		amount = 0
	}
	return int64(math.Round(amount * 100))
}

// asFloat coerces a JSON value that may be a number or a numeric string into a
// float. Provider APIs are inconsistent: DigitalOcean returns money as strings,
// Vultr and Linode as numbers.
func asFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case json.Number:
		return t.Float64()
	case string:
		if t == "" {
			return 0, nil
		}
		return strconv.ParseFloat(t, 64)
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("unexpected numeric type %T", v)
	}
}

// --- DigitalOcean -----------------------------------------------------------
//
// GET https://api.digitalocean.com/v2/customers/my/balance
// Authorization: Bearer <token>
//   { "month_to_date_balance":"23.44", "account_balance":"0.00",
//     "month_to_date_usage":"23.44", "generated_at":"2024-..." }
// We report month_to_date_usage, the accrued spend for the current month.

type digitalOcean struct{}

func (digitalOcean) Name() string { return ProviderDigitalOcean }

func (digitalOcean) Request(ctx context.Context, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.digitalocean.com/v2/customers/my/balance", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (digitalOcean) Parse(body []byte) (int64, string, error) {
	var r struct {
		MonthToDateUsage any `json:"month_to_date_usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, "", err
	}
	usage, err := asFloat(r.MonthToDateUsage)
	if err != nil {
		return 0, "", err
	}
	return toCents(usage), "USD", nil
}

// --- Vultr ------------------------------------------------------------------
//
// GET https://api.vultr.com/v2/account
// Authorization: Bearer <token>
//   { "account": { "balance": -10.00, "pending_charges": 5.67, ... } }
// pending_charges is the accrued, not-yet-invoiced spend for the period.

type vultr struct{}

func (vultr) Name() string { return ProviderVultr }

func (vultr) Request(ctx context.Context, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.vultr.com/v2/account", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (vultr) Parse(body []byte) (int64, string, error) {
	var r struct {
		Account struct {
			PendingCharges any `json:"pending_charges"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, "", err
	}
	pending, err := asFloat(r.Account.PendingCharges)
	if err != nil {
		return 0, "", err
	}
	return toCents(pending), "USD", nil
}

// --- Linode -----------------------------------------------------------------
//
// GET https://api.linode.com/v4/account
// Authorization: Bearer <token>
//   { "balance": 0.0, "balance_uninvoiced": 12.50, ... }
// balance_uninvoiced is the accrued spend not yet on an invoice.

type linode struct{}

func (linode) Name() string { return ProviderLinode }

func (linode) Request(ctx context.Context, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.linode.com/v4/account", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (linode) Parse(body []byte) (int64, string, error) {
	var r struct {
		BalanceUninvoiced any `json:"balance_uninvoiced"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, "", err
	}
	uninvoiced, err := asFloat(r.BalanceUninvoiced)
	if err != nil {
		return 0, "", err
	}
	return toCents(uninvoiced), "USD", nil
}

// --- Hetzner ----------------------------------------------------------------
//
// Hetzner Cloud bills per-resource. We sum the current month-to-date cost of
// every server from the Cloud API's pricing-bearing server list:
//
// GET https://api.hetzner.cloud/v1/servers
// Authorization: Bearer <token>
//   { "servers": [ { "server_type": { "prices": [
//       { "location":"fsn1", "price_monthly": { "gross":"4.5100000000" } } ] },
//     "datacenter": { "location": { "name":"fsn1" } } } ] }
// Hetzner prices are EUR gross. We approximate month-to-date as the prorated
// monthly price; the daily job runs this each day so the figure tracks the
// running fleet.

type hetzner struct{}

func (hetzner) Name() string { return ProviderHetzner }

func (hetzner) Request(ctx context.Context, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.hetzner.cloud/v1/servers", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (hetzner) Parse(body []byte) (int64, string, error) {
	var r struct {
		Servers []struct {
			ServerType struct {
				Prices []struct {
					Location     string `json:"location"`
					PriceMonthly struct {
						Gross any `json:"gross"`
					} `json:"price_monthly"`
				} `json:"prices"`
			} `json:"server_type"`
			Datacenter struct {
				Location struct {
					Name string `json:"name"`
				} `json:"location"`
			} `json:"datacenter"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, "", err
	}
	var total float64
	for _, srv := range r.Servers {
		loc := srv.Datacenter.Location.Name
		price, ok := hetznerPriceForLocation(srv.ServerType.Prices, loc)
		if !ok {
			continue
		}
		total += price
	}
	return toCents(total), "EUR", nil
}

// hetznerPriceForLocation picks the monthly gross price matching the server's
// datacenter location, falling back to the first listed price so a server in an
// unlisted location still contributes a defensible estimate.
func hetznerPriceForLocation(prices []struct {
	Location     string `json:"location"`
	PriceMonthly struct {
		Gross any `json:"gross"`
	} `json:"price_monthly"`
}, loc string) (float64, bool) {
	// Deterministic order so the fallback is stable regardless of API ordering.
	sort.SliceStable(prices, func(i, j int) bool { return prices[i].Location < prices[j].Location })
	for _, p := range prices {
		if p.Location == loc {
			f, err := asFloat(p.PriceMonthly.Gross)
			return f, err == nil
		}
	}
	if len(prices) > 0 {
		f, err := asFloat(prices[0].PriceMonthly.Gross)
		return f, err == nil
	}
	return 0, false
}
