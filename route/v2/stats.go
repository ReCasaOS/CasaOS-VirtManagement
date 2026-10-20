package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// The stats routes answer bare bodies (no data envelope) and errors as the
// code alone, never the cause. Answers are live: never cached.
//
// The id comes from the generated binder, which unescapes what echo already
// unescaped, so `vm%253A<uuid>` arrives as `vm:<uuid>`. Harmless: the lookup
// is an exact key, ring files are named by a hash, and an id holds no `%`.

// maxRulesBody bounds a PUT of rules; two rules take about 150 bytes.
const maxRulesBody = 64 << 10

func statsCode(ctx echo.Context, status int, code string) error {
	return ctx.JSON(status, codegen.BaseResponse{Message: &code})
}

func badRules(ctx echo.Context, field string) error {
	return statsCode(ctx, http.StatusBadRequest, "bad_rules: "+field)
}

func statsFailure(ctx echo.Context, err error) error {
	var re *stats.RuleError
	switch {
	case errors.Is(err, service.ErrUnknownItem):
		return statsCode(ctx, http.StatusNotFound, "unknown_item")
	case errors.As(err, &re):
		return badRules(ctx, fmt.Sprintf("rules[%d].%s", re.Index, re.Field))
	case !errors.Is(err, service.ErrStatsUnavailable):
		logger.Error("reading VM stats failed", zap.Error(err))
	}
	return statsDown(ctx)
}

// statsService is the sampler, nil when none runs; it marks the answer not
// to be cached.
func (v *VirtManagement) statsService(ctx echo.Context) *service.Stats {
	ctx.Response().Header().Set("Cache-Control", "no-store")
	return v.stats
}

func statsDown(ctx echo.Context) error {
	return statsCode(ctx, http.StatusServiceUnavailable, "stats_unavailable")
}

// ListStats lists the VMs with their latest use.
func (v *VirtManagement) ListStats(ctx echo.Context) error {
	s := v.statsService(ctx)
	if s == nil {
		return statsDown(ctx)
	}
	items, err := s.Items()
	if err != nil {
		return statsFailure(ctx, err)
	}
	return ctx.JSON(http.StatusOK, struct {
		Items []service.StatsItem `json:"items"`
		Notes []string            `json:"notes"`
	}{items, s.Notes()})
}

// GetStatsSeries returns one VM's history over range (live when absent).
func (v *VirtManagement) GetStatsSeries(ctx echo.Context, id codegen.StatsID, _ codegen.GetStatsSeriesParams) error {
	s := v.statsService(ctx)
	if s == nil {
		return statsDown(ctx)
	}
	r := stats.RangeLive
	// read here, not from the bound params: `?range=` must be refused, not
	// taken for absent. A repeated range never gets here: the binder refuses it.
	if values, ok := ctx.QueryParams()["range"]; ok {
		var valid bool
		if r, valid = stats.ParseRange(values[0]); !valid {
			return statsCode(ctx, http.StatusBadRequest, "bad_range")
		}
	}
	series, err := s.Series(id, r)
	if err != nil {
		return statsFailure(ctx, err)
	}
	return ctx.JSON(http.StatusOK, series)
}

func statsAlerts(ctx echo.Context, s *service.Stats, id string) error {
	rules, active, err := s.Alerts(id)
	if err != nil {
		return statsFailure(ctx, err)
	}
	return ctx.JSON(http.StatusOK, struct {
		Rules  []stats.Rule         `json:"rules"`
		Active []service.StatsAlert `json:"active"`
	}{rules, active})
}

// GetStatsAlerts returns the rules of a VM and its firing alerts.
func (v *VirtManagement) GetStatsAlerts(ctx echo.Context, id codegen.StatsID) error {
	s := v.statsService(ctx)
	if s == nil {
		return statsDown(ctx)
	}
	return statsAlerts(ctx, s, id)
}

// SetStatsAlerts updates the rules sent for a VM. The request validator skips
// this route (route/v2.go): every malformed body is bad_rules naming the
// field, and the body is bounded before it is read.
func (v *VirtManagement) SetStatsAlerts(ctx echo.Context, id codegen.StatsID) error {
	s := v.statsService(ctx)
	if s == nil {
		return statsDown(ctx)
	}
	raw, err := io.ReadAll(http.MaxBytesReader(ctx.Response(), ctx.Request().Body, maxRulesBody))
	var body struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Rules == nil {
		return badRules(ctx, "rules") // also too large, not JSON, not an object, no list
	}
	rules := make([]stats.Rule, len(body.Rules))
	for i, m := range body.Rules {
		// each field present (an absent one is empty, which Unmarshal refuses)
		// and of its type, null refused; the values are Common's to judge.
		for _, f := range []struct {
			name string
			to   any
		}{{"key", &rules[i].Key}, {"enabled", &rules[i].Enabled}, {"threshold", &rules[i].Threshold}, {"minutes", &rules[i].Minutes}} {
			if v := m[f.name]; string(v) == "null" || json.Unmarshal(v, f.to) != nil {
				return badRules(ctx, fmt.Sprintf("rules[%d].%s", i, f.name))
			}
		}
	}
	if err := s.SetRules(id, rules); err != nil {
		return statsFailure(ctx, err)
	}
	return statsAlerts(ctx, s, id)
}
