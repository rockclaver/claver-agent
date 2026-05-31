package cost

import (
	"bytes"
	"encoding/csv"
	"strconv"
)

// ExportCSV renders the month's cost rollup as a single CSV document. One
// "section" column distinguishes the row kinds so the whole rollup — summary,
// per-project AI spend, and per-provider infra — fits one downloadable file.
func (c *Calculator) ExportCSV(month string) (string, error) {
	r, err := c.Rollup(month)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{
		"section", "name", "sessions", "input_tokens", "output_tokens",
		"cache_tokens", "tool_calls", "ai_spend_usd", "infra_amount", "currency", "status",
	})

	// Summary block.
	_ = w.Write([]string{"summary", "month", "", "", "", "", "", "", "", r.Month, ""})
	_ = w.Write([]string{"summary", "total_ai_spend", "", "", "", "", "",
		dollars(r.TotalAISpendCents), "", "USD", ""})
	infraStatus := "ok"
	if !r.InfraAvailable {
		infraStatus = "unavailable"
	}
	_ = w.Write([]string{"summary", "total_infra_spend", "", "", "", "", "", "",
		dollars(r.TotalInfraSpendCents), r.InfraCurrency, infraStatus})
	_ = w.Write([]string{"summary", "prs_shipped", strconv.Itoa(r.PRsShipped),
		"", "", "", "", "", "", "", ""})
	_ = w.Write([]string{"summary", "ai_cost_per_pr", "", "", "", "", "",
		dollars(r.CostPerPRCents), "", "USD", ""})

	// Per-project AI spend.
	for _, p := range r.Projects {
		name := p.ProjectName
		if name == "" {
			name = p.ProjectID
		}
		_ = w.Write([]string{
			"project", name,
			strconv.Itoa(p.Sessions),
			strconv.Itoa(p.InputTokens),
			strconv.Itoa(p.OutputTokens),
			strconv.Itoa(p.CacheTokens),
			strconv.Itoa(p.ToolCalls),
			dollars(p.AISpendCents),
			"", "USD", "",
		})
	}

	// Per-provider infra spend.
	for _, p := range r.Infra {
		_ = w.Write([]string{
			"infra", p.Provider, "", "", "", "", "", "",
			dollars(p.AmountCents), p.Currency, p.Status,
		})
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return buf.String(), nil
}
