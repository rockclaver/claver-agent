// Package cost rolls per-session AI token spend and per-server infrastructure
// bills into a single cross-fleet dashboard, whose headline metric is AI cost
// per PR shipped (issue #60). AI spend is priced from the token usage package
// sessions records; infrastructure spend comes from package billing. The PR
// denominator is the project journal's "pr" entries (issues #5/#7).
package cost

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/rockclaver/claver/agent/internal/store"
)

// journalKindPR is the project-journal entry kind that counts as a shipped PR.
// It mirrors memory.JournalPR; the cost layer keeps a local copy rather than
// importing the memory package just for one string.
const journalKindPR = "pr"

// AgentRate prices one agent's tokens in cents per one million tokens. Cache
// reads are billed far below fresh input, so they are tracked separately.
type AgentRate struct {
	InputCentsPerM  int64
	OutputCentsPerM int64
	CacheCentsPerM  int64
}

// DefaultPricing returns shipped list prices (cents per million tokens). They
// are deliberately conservative defaults; an operator can override them.
func DefaultPricing() map[string]AgentRate {
	return map[string]AgentRate{
		// Claude (Opus-class list price): $15 / $75 per Mtok, cache reads $1.50.
		"claude": {InputCentsPerM: 1500, OutputCentsPerM: 7500, CacheCentsPerM: 150},
		// Codex (GPT-class list price): $2.50 / $10 per Mtok, cache reads $0.25.
		"codex": {InputCentsPerM: 250, OutputCentsPerM: 1000, CacheCentsPerM: 25},
	}
}

// Calculator computes a cost rollup from the State Store.
type Calculator struct {
	Store    *store.Store
	ServerID string
	Pricing  map[string]AgentRate
	Now      func() time.Time
}

// New constructs a Calculator with default pricing.
func New(st *store.Store, serverID string) *Calculator {
	if serverID == "" {
		serverID = "local"
	}
	return &Calculator{Store: st, ServerID: serverID, Pricing: DefaultPricing(), Now: time.Now}
}

// ProjectCost is one project's usage and priced AI spend for the month.
type ProjectCost struct {
	ProjectID    string `json:"project_id"`
	ProjectName  string `json:"project_name"`
	Sessions     int    `json:"sessions"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CacheTokens  int    `json:"cache_tokens"`
	ToolCalls    int    `json:"tool_calls"`
	AISpendCents int64  `json:"ai_spend_cents"`
}

// ProviderCost is one provider's normalized monthly infra bill.
type ProviderCost struct {
	Provider    string `json:"provider"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	Detail      string `json:"detail,omitempty"`
}

// Rollup is the full cost picture for one month on one server.
type Rollup struct {
	Month       string `json:"month"`
	ServerID    string `json:"server_id"`
	GeneratedAt int64  `json:"generated_at"`

	TotalAISpendCents int64 `json:"total_ai_spend_cents"`

	TotalInfraSpendCents int64    `json:"total_infra_spend_cents"`
	InfraCurrency        string   `json:"infra_currency"`
	InfraAvailable       bool     `json:"infra_available"`
	InfraUnavailable     []string `json:"infra_unavailable"`

	PRsShipped     int   `json:"prs_shipped"`
	CostPerPRCents int64 `json:"cost_per_pr_cents"`

	Projects []ProjectCost  `json:"projects"`
	Infra    []ProviderCost `json:"infra"`
}

// monthBounds returns [start, end) for the "2006-01" month string, in UTC.
func monthBounds(month string) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01", month, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid month %q: %w", month, err)
	}
	return start, start.AddDate(0, 1, 0), nil
}

// Rollup computes the cost rollup for month ("2006-01"); an empty month uses
// the current UTC month.
func (c *Calculator) Rollup(month string) (Rollup, error) {
	if month == "" {
		month = c.Now().UTC().Format("2006-01")
	}
	start, end, err := monthBounds(month)
	if err != nil {
		return Rollup{}, err
	}

	r := Rollup{
		Month:            month,
		ServerID:         c.ServerID,
		GeneratedAt:      c.Now().Unix(),
		InfraUnavailable: []string{},
		Projects:         []ProjectCost{},
		Infra:            []ProviderCost{},
	}

	names, err := c.projectNames()
	if err != nil {
		return Rollup{}, err
	}

	sessions, err := c.Store.ListSessions("")
	if err != nil {
		return Rollup{}, err
	}
	byProject := map[string]*ProjectCost{}
	var order []string
	for _, s := range sessions {
		if s.StartedAt.Before(start) || !s.StartedAt.Before(end) {
			continue
		}
		pc, ok := byProject[s.ProjectID]
		if !ok {
			pc = &ProjectCost{ProjectID: s.ProjectID, ProjectName: names[s.ProjectID]}
			byProject[s.ProjectID] = pc
			order = append(order, s.ProjectID)
		}
		pc.Sessions++
		pc.InputTokens += s.InputTokens
		pc.OutputTokens += s.OutputTokens
		pc.CacheTokens += s.CacheTokens
		pc.ToolCalls += s.ToolCalls
		spend := c.sessionSpendCents(s)
		pc.AISpendCents += spend
		r.TotalAISpendCents += spend
	}
	for _, id := range order {
		r.Projects = append(r.Projects, *byProject[id])
	}

	// PRs shipped this month → the cost-per-PR denominator.
	prs, err := c.Store.CountJournalEntries("", journalKindPR, start, end)
	if err != nil {
		return Rollup{}, err
	}
	r.PRsShipped = prs
	if prs > 0 {
		r.CostPerPRCents = int64(math.Round(float64(r.TotalAISpendCents) / float64(prs)))
	}

	// Infrastructure spend, with graceful degradation.
	infra, err := c.Store.ListInfraCost(month)
	if err != nil {
		return Rollup{}, err
	}
	c.foldInfra(&r, infra)
	return r, nil
}

// foldInfra sums the infra rows for this server into the rollup, tracking which
// providers degraded so the UI can show "infra cost unavailable" instead of a
// blank panel.
func (c *Calculator) foldInfra(r *Rollup, rows []store.InfraCost) {
	for _, row := range rows {
		if row.ServerID != c.ServerID {
			continue
		}
		r.Infra = append(r.Infra, ProviderCost{
			Provider:    row.Provider,
			AmountCents: row.AmountCents,
			Currency:    row.Currency,
			Status:      row.Status,
			Detail:      row.Detail,
		})
		if row.Status != "ok" {
			r.InfraUnavailable = append(r.InfraUnavailable, row.Provider)
			continue
		}
		r.TotalInfraSpendCents += row.AmountCents
		if r.InfraCurrency == "" {
			r.InfraCurrency = row.Currency
		} else if r.InfraCurrency != row.Currency {
			// Mixed-currency fleets can't be summed into one figure honestly.
			r.InfraCurrency = "mixed"
		}
	}
	// Infra is "available" when at least one provider reported a real figure.
	r.InfraAvailable = r.TotalInfraSpendCentsHasOK()
	if r.InfraCurrency == "" {
		r.InfraCurrency = "USD"
	}
}

// TotalInfraSpendCentsHasOK reports whether any infra provider returned an "ok"
// row. (A zero total can still be available — a $0 month is valid.)
func (r *Rollup) TotalInfraSpendCentsHasOK() bool {
	for _, p := range r.Infra {
		if p.Status == "ok" {
			return true
		}
	}
	return false
}

// sessionSpendCents prices one session's tokens using its agent's rate. An
// unknown agent falls back to the claude rate so spend is never undercounted.
func (c *Calculator) sessionSpendCents(s store.Session) int64 {
	rate, ok := c.Pricing[s.Agent]
	if !ok {
		rate = c.Pricing["claude"]
	}
	return tokenCost(s.InputTokens, rate.InputCentsPerM) +
		tokenCost(s.OutputTokens, rate.OutputCentsPerM) +
		tokenCost(s.CacheTokens, rate.CacheCentsPerM)
}

func tokenCost(tokens int, rateCentsPerM int64) int64 {
	if tokens <= 0 || rateCentsPerM <= 0 {
		return 0
	}
	return int64(math.Round(float64(tokens) * float64(rateCentsPerM) / 1_000_000))
}

func (c *Calculator) projectNames() (map[string]string, error) {
	projects, err := c.Store.ListProjects()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	return names, nil
}

// dollars formats integer cents as a fixed-2 decimal string.
func dollars(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
	if neg {
		return "-" + s
	}
	return s
}
