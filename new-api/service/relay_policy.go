package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// Published prices and limits. The price file stays the base table; a
// published version replaces one existing route from its effective time. The
// log is append-only (model.RelayPolicyVersion). RELAY_POLICY_VERSIONS=false
// (default) keeps the previous file-only behaviour and never reads the log.

var ErrRelayPolicyDisabled = errors.New("published price and limit versions are disabled")

const (
	// RelayPolicyBaseLimits names the built-in limits used until one is published.
	RelayPolicyBaseLimits  = "builtin-1-5"
	relayPolicyLimitsScope = "global"
	relayPolicyMaxGlobal   = 20
	// Every process re-reads the log this often. A version is published at
	// least model.RelayPolicyLeadSeconds ahead, so all processes switch at the
	// same effective second. A log older than relayPolicyStale is not trusted.
	relayPolicyRefresh = 5 * time.Second
	relayPolicyStale   = 30 * time.Second
)

// RelayWalletRoute exposes the route type to the administrator API only.
type RelayWalletRoute = relayWalletChatRoute

func RelayPolicyEnabled() bool {
	return common.GetEnvOrDefaultBool("RELAY_POLICY_VERSIONS", false)
}

var relayPolicyCache struct {
	sync.Mutex
	rows   []model.RelayPolicyVersion
	loaded time.Time
	valid  bool
}

// ResetRelayPolicyCache forces the next read to consult the database.
func ResetRelayPolicyCache() {
	relayPolicyCache.Lock()
	relayPolicyCache.valid = false
	relayPolicyCache.rows = nil
	relayPolicyCache.Unlock()
}

func relayPolicyRows() ([]model.RelayPolicyVersion, error) {
	relayPolicyCache.Lock()
	defer relayPolicyCache.Unlock()
	now := time.Now()
	if relayPolicyCache.valid && now.Sub(relayPolicyCache.loaded) < relayPolicyRefresh {
		return relayPolicyCache.rows, nil
	}
	if model.DB == nil {
		return nil, ErrRelayPolicyDisabled
	}
	rows, err := model.ListRelayPolicy(model.DB)
	if err != nil {
		// A short database hiccup may reuse a recent log; an old one may miss a
		// version that is already in force, so pricing fails closed instead.
		if relayPolicyCache.valid && now.Sub(relayPolicyCache.loaded) < relayPolicyStale {
			return relayPolicyCache.rows, nil
		}
		return nil, err
	}
	relayPolicyCache.rows, relayPolicyCache.loaded, relayPolicyCache.valid = rows, now, true
	return rows, nil
}

func relayPolicyPriceScope(channelID int, requestModel string) string {
	return strconv.Itoa(channelID) + "/" + requestModel
}

// relayPolicySplit returns the newest row of a scope already in force and the
// rows that are published but not yet effective, oldest first.
func relayPolicySplit(rows []model.RelayPolicyVersion, kind, scope string, now int64) (*model.RelayPolicyVersion, []model.RelayPolicyVersion) {
	var active *model.RelayPolicyVersion
	var pending []model.RelayPolicyVersion
	for i := range rows {
		if rows[i].Kind != kind || rows[i].Scope != scope {
			continue
		}
		if rows[i].EffectiveAt <= now {
			active = &rows[i]
		} else {
			pending = append(pending, rows[i])
		}
	}
	return active, pending
}

func relayPolicyDecode(payload string, value any) error {
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return model.ErrRelayInput
	}
	return nil
}

// A stored payload is validated again on every use: a row that no longer
// satisfies the rules stops paid chat rather than price with it.
func relayPolicyRoute(row model.RelayPolicyVersion, base relayWalletChatRoute) (relayWalletChatRoute, error) {
	var route relayWalletChatRoute
	if err := relayPolicyDecode(row.Payload, &route); err != nil {
		return route, err
	}
	if err := route.validate(); err != nil {
		return route, model.ErrRelayInput
	}
	if route.Version != row.Version || route.ChannelID != base.ChannelID || route.RequestModel != base.RequestModel ||
		relayPolicyPriceScope(route.ChannelID, route.RequestModel) != row.Scope || !relayPolicySameIdentity(route, base) {
		return route, model.ErrRelayInput
	}
	return route, nil
}

// Publication may change rates and the per-request input/output limits. What
// the operator verified about the provider model itself is not republishable.
func relayPolicySameIdentity(route, base relayWalletChatRoute) bool {
	return route.Model == base.Model && route.Schema == base.Schema && route.Unit == base.Unit &&
		route.OutputCap == base.OutputCap && route.Thinking == base.Thinking && route.ContextTokens == base.ContextTokens
}

// activeRelayWalletRoutes is the table that prices a new request right now.
func activeRelayWalletRoutes() (map[string]relayWalletChatRoute, error) {
	base, err := loadRelayWalletRoutes()
	if err != nil || !RelayPolicyEnabled() {
		return base, err
	}
	rows, err := relayPolicyRows()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	routes := make(map[string]relayWalletChatRoute, len(base))
	for key, route := range base {
		if active, _ := relayPolicySplit(rows, model.RelayPolicyPrice, relayPolicyPriceScope(route.ChannelID, route.RequestModel), now); active != nil {
			published, err := relayPolicyRoute(*active, route)
			if err != nil {
				return nil, err
			}
			route = published
		}
		routes[key] = route
	}
	return routes, nil
}

type relayPolicyLimits struct {
	Global int `json:"global"`
}

func relayPolicyLimitsValue(row model.RelayPolicyVersion) (int, error) {
	var limits relayPolicyLimits
	if err := relayPolicyDecode(row.Payload, &limits); err != nil {
		return 0, err
	}
	if limits.Global < 1 || limits.Global > relayPolicyMaxGlobal {
		return 0, model.ErrRelayInput
	}
	return limits.Global, nil
}

// ActiveRelayWalletLimits: one generation per user is a product rule and not
// adjustable; the site-wide limit may be republished within 1..20.
func ActiveRelayWalletLimits() (model.RelayWalletLimits, error) {
	limits := model.RelayWalletLimits{PerUser: 1, Global: 5}
	if !RelayPolicyEnabled() {
		return limits, nil
	}
	rows, err := relayPolicyRows()
	if err != nil {
		return limits, err
	}
	if active, _ := relayPolicySplit(rows, model.RelayPolicyLimits, relayPolicyLimitsScope, time.Now().Unix()); active != nil {
		global, err := relayPolicyLimitsValue(*active)
		if err != nil {
			return limits, err
		}
		limits.Global = global
	}
	return limits, nil
}

// RelayPricePublication is the exact operation an administrator confirms with
// a second factor: the complete new route, when it starts and why.
type RelayPricePublication struct {
	ExpectedVersion string           `json:"expected_version"`
	EffectiveAt     int64            `json:"effective_at"`
	Reason          string           `json:"reason"`
	Route           RelayWalletRoute `json:"route"`
}

type RelayLimitsPublication struct {
	ExpectedVersion string `json:"expected_version"`
	Version         string `json:"version"`
	EffectiveAt     int64  `json:"effective_at"`
	Reason          string `json:"reason"`
	Global          int    `json:"global"`
}

func relayPolicyAppend(row model.RelayPolicyVersion, base string, payload any) (model.RelayPolicyVersion, error) {
	raw, err := common.Marshal(payload)
	if err != nil {
		return model.RelayPolicyVersion{}, err
	}
	digest := sha256.Sum256(raw)
	row.Payload, row.Digest = string(raw), hex.EncodeToString(digest[:])
	saved, err := model.PublishRelayPolicy(model.DB, row, base, time.Now())
	if err == nil {
		ResetRelayPolicyCache()
	}
	return saved, err
}

// PublishRelayPrice appends a new price version for an existing route.
func PublishRelayPrice(operatorID int, input RelayPricePublication) (model.RelayPolicyVersion, error) {
	if !RelayPolicyEnabled() || model.DB == nil {
		return model.RelayPolicyVersion{}, ErrRelayPolicyDisabled
	}
	base, err := loadRelayWalletRoutes()
	if err != nil {
		return model.RelayPolicyVersion{}, ErrRelayPolicyDisabled
	}
	route := input.Route
	if route.validate() != nil || len(route.RequestModel) > 120 {
		return model.RelayPolicyVersion{}, model.ErrRelayInput
	}
	current, ok := base[relayWalletRouteKey(route.ChannelID, route.RequestModel)]
	if !ok || !relayPolicySameIdentity(route, current) {
		return model.RelayPolicyVersion{}, model.ErrRelayInput
	}
	for _, fileRoute := range base {
		if fileRoute.Version == route.Version {
			return model.RelayPolicyVersion{}, model.ErrRelayConflict
		}
	}
	return relayPolicyAppend(model.RelayPolicyVersion{Kind: model.RelayPolicyPrice, Scope: relayPolicyPriceScope(route.ChannelID, route.RequestModel),
		Previous: input.ExpectedVersion, Version: route.Version, OperatorID: operatorID, Reason: input.Reason, EffectiveAt: input.EffectiveAt},
		current.Version, route)
}

// PublishRelayLimits appends a new site-wide generation limit.
func PublishRelayLimits(operatorID int, input RelayLimitsPublication) (model.RelayPolicyVersion, error) {
	if !RelayPolicyEnabled() || model.DB == nil {
		return model.RelayPolicyVersion{}, ErrRelayPolicyDisabled
	}
	if input.Global < 1 || input.Global > relayPolicyMaxGlobal {
		return model.RelayPolicyVersion{}, model.ErrRelayInput
	}
	return relayPolicyAppend(model.RelayPolicyVersion{Kind: model.RelayPolicyLimits, Scope: relayPolicyLimitsScope,
		Previous: input.ExpectedVersion, Version: input.Version, OperatorID: operatorID, Reason: input.Reason, EffectiveAt: input.EffectiveAt},
		RelayPolicyBaseLimits, relayPolicyLimits{Global: input.Global})
}

// The administrator view. Rates are what this process actually loaded and
// uses, including upstream cost; it never leaves the administrator API.
type RelayPolicyRouteVersion struct {
	Version     string           `json:"version"`
	Source      string           `json:"source"` // "file" or "published"
	EffectiveAt int64            `json:"effective_at"`
	Route       RelayWalletRoute `json:"route"`
}

type RelayPolicyRouteView struct {
	ChannelID    int                       `json:"channel_id"`
	RequestModel string                    `json:"request_model"`
	Active       RelayPolicyRouteVersion   `json:"active"`
	Pending      []RelayPolicyRouteVersion `json:"pending"`
}

type RelayPolicyLimitVersion struct {
	Version     string `json:"version"`
	Source      string `json:"source"` // "builtin" or "published"
	EffectiveAt int64  `json:"effective_at"`
	Global      int    `json:"global"`
}

type RelayPolicyView struct {
	ServerTime  int64                      `json:"server_time"`
	LeadSeconds int                        `json:"lead_seconds"`
	PerUser     int                        `json:"per_user"`
	MaxGlobal   int                        `json:"max_global"`
	Limits      RelayPolicyLimitVersion    `json:"limits"`
	LimitsNext  []RelayPolicyLimitVersion  `json:"limits_pending"`
	Routes      []RelayPolicyRouteView     `json:"routes"`
	History     []model.RelayPolicyVersion `json:"history"`
}

func ReadRelayPolicy() (RelayPolicyView, error) {
	view := RelayPolicyView{LeadSeconds: model.RelayPolicyLeadSeconds, PerUser: 1, MaxGlobal: relayPolicyMaxGlobal,
		Limits: RelayPolicyLimitVersion{Version: RelayPolicyBaseLimits, Source: "builtin", Global: 5},
		LimitsNext: []RelayPolicyLimitVersion{}, Routes: []RelayPolicyRouteView{}, History: []model.RelayPolicyVersion{}}
	if !RelayPolicyEnabled() || model.DB == nil {
		return view, ErrRelayPolicyDisabled
	}
	base, err := loadRelayWalletRoutes()
	if err != nil {
		return view, ErrRelayPolicyDisabled
	}
	// The administrator view always reads the database, never the cache.
	rows, err := model.ListRelayPolicy(model.DB)
	if err != nil {
		return view, err
	}
	now := time.Now().Unix()
	view.ServerTime = now
	for _, route := range base {
		item := RelayPolicyRouteView{ChannelID: route.ChannelID, RequestModel: route.RequestModel, Pending: []RelayPolicyRouteVersion{},
			Active: RelayPolicyRouteVersion{Version: route.Version, Source: "file", Route: route}}
		active, pending := relayPolicySplit(rows, model.RelayPolicyPrice, relayPolicyPriceScope(route.ChannelID, route.RequestModel), now)
		if active != nil {
			published, err := relayPolicyRoute(*active, route)
			if err != nil {
				return view, err
			}
			item.Active = RelayPolicyRouteVersion{Version: active.Version, Source: "published", EffectiveAt: active.EffectiveAt, Route: published}
		}
		for _, row := range pending {
			published, err := relayPolicyRoute(row, route)
			if err != nil {
				return view, err
			}
			item.Pending = append(item.Pending, RelayPolicyRouteVersion{Version: row.Version, Source: "published", EffectiveAt: row.EffectiveAt, Route: published})
		}
		view.Routes = append(view.Routes, item)
	}
	sort.Slice(view.Routes, func(i, j int) bool {
		if view.Routes[i].RequestModel != view.Routes[j].RequestModel {
			return view.Routes[i].RequestModel < view.Routes[j].RequestModel
		}
		return view.Routes[i].ChannelID < view.Routes[j].ChannelID
	})
	active, pending := relayPolicySplit(rows, model.RelayPolicyLimits, relayPolicyLimitsScope, now)
	if active != nil {
		global, err := relayPolicyLimitsValue(*active)
		if err != nil {
			return view, err
		}
		view.Limits = RelayPolicyLimitVersion{Version: active.Version, Source: "published", EffectiveAt: active.EffectiveAt, Global: global}
	}
	for _, row := range pending {
		global, err := relayPolicyLimitsValue(row)
		if err != nil {
			return view, err
		}
		view.LimitsNext = append(view.LimitsNext, RelayPolicyLimitVersion{Version: row.Version, Source: "published", EffectiveAt: row.EffectiveAt, Global: global})
	}
	// Newest first; the log is small and bounded (model.relayPolicyMaxRows).
	for i := len(rows) - 1; i >= 0 && len(view.History) < 100; i-- {
		view.History = append(view.History, rows[i])
	}
	return view, nil
}
