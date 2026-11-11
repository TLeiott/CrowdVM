package server

// GET /provider-models: the harness/model catalog. It is the guest's built-in
// catalog unless a server admin replaced it (PUT /admin/provider-models),
// e.g. to add a model before a new guest ships. Windows servers serve an
// embedded snapshot of the built-in catalog.

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	RegisterFeature("provider_catalog", func(s *Server) error {
		s.API("GET /provider-models", ActionAny, s.handleProviderModels)
		s.API("GET /admin/provider-models", ActionAdmin, s.handleAdminCatalogGet)
		s.API("PUT /admin/provider-models", ActionAdmin, s.handleAdminCatalogPut)
		s.API("DELETE /admin/provider-models", ActionAdmin, s.handleAdminCatalogDelete)
		return nil
	})
}

const catalogSetting = "provider_models.override"

// catProvider / catModel are the fields of the catalog the server reads
// (the served JSON keeps every field).
type catProvider struct {
	Provider     string     `json:"provider"`
	Name         string     `json:"name"`
	Aliases      []string   `json:"aliases,omitempty"`
	DefaultModel string     `json:"defaultModel,omitempty"`
	Models       []catModel `json:"models"`
}

type catModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	FastMode bool   `json:"fastMode"`
}

// getSetting / putSetting read and write the settings table ("" = unset).
func (s *Server) getSetting(key string) string {
	var v string
	_ = s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Server) putSetting(key, value string) error {
	if value == "" {
		_, err := s.DB.Exec(`DELETE FROM settings WHERE key=?`, key)
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// catalogRaw returns the effective catalog JSON and its source.
func (s *Server) catalogRaw() (json.RawMessage, string) {
	if v := s.getSetting(catalogSetting); v != "" {
		var o struct {
			Providers json.RawMessage `json:"providers"`
		}
		if json.Unmarshal([]byte(v), &o) == nil && len(o.Providers) > 0 {
			return o.Providers, "admin"
		}
	}
	return builtinCatalog(), "builtin"
}

// providerCatalog parses the effective catalog.
func (s *Server) providerCatalog() []catProvider {
	raw, _ := s.catalogRaw()
	var out []catProvider
	_ = json.Unmarshal(raw, &out)
	return out
}

// catalogProvider resolves a provider id or alias.
func (s *Server) catalogProvider(id string) (catProvider, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range s.providerCatalog() {
		if p.Provider == id || slices.Contains(p.Aliases, id) {
			return p, true
		}
	}
	return catProvider{}, false
}

func (p catProvider) model(id string) (catModel, bool) {
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return catModel{}, false
}

func (s *Server) handleProviderModels(c *Ctx) error {
	raw, _ := s.catalogRaw()
	var list any
	_ = json.Unmarshal(raw, &list)
	if list == nil {
		list = []any{}
	}
	return c.OK(200, "provider.models", map[string]any{"providers": s.aiDecorateProviders(c.P, list)})
}

func (s *Server) catalogJSON() map[string]any {
	raw, src := s.catalogRaw()
	var list any
	_ = json.Unmarshal(raw, &list)
	return map[string]any{"providers": list, "source": src, "builtin": json.RawMessage(builtinCatalog())}
}

func (s *Server) handleAdminCatalogGet(c *Ctx) error {
	return c.OK(200, "provider.catalog", s.catalogJSON())
}

func (s *Server) handleAdminCatalogPut(c *Ctx) error {
	var req struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if len(req.Providers) == 0 {
		return Err(400, "invalid_catalog", "providers must list at least one harness (DELETE resets to the built-in catalog).")
	}
	b, _ := json.Marshal(req.Providers)
	var typed []catProvider
	if err := json.Unmarshal(b, &typed); err != nil {
		return Err(400, "invalid_catalog", "providers: "+err.Error())
	}
	seen := map[string]bool{}
	for i, p := range typed {
		if p.Provider == "" || p.Name == "" {
			return ErrD(400, "invalid_catalog", "Every provider needs provider and name.", map[string]any{"index": i})
		}
		if seen[p.Provider] {
			return ErrD(400, "invalid_catalog", "Duplicate provider "+p.Provider+".", map[string]any{"index": i})
		}
		seen[p.Provider] = true
		ids := map[string]bool{}
		for _, m := range p.Models {
			if m.ID == "" || ids[m.ID] {
				return ErrD(400, "invalid_catalog", "Models of "+p.Provider+" need unique ids.", map[string]any{"index": i})
			}
			ids[m.ID] = true
		}
		if p.DefaultModel != "" && !ids[p.DefaultModel] {
			return ErrD(400, "invalid_catalog", "defaultModel of "+p.Provider+" is not one of its models.", map[string]any{"index": i})
		}
	}
	if err := s.putSetting(catalogSetting, mustJSON(map[string]any{"providers": req.Providers, "updatedAt": store.Now(), "updatedBy": c.P.User.ID})); err != nil {
		return err
	}
	return c.OK(200, "provider.catalog", s.catalogJSON())
}

func (s *Server) handleAdminCatalogDelete(c *Ctx) error {
	if err := s.putSetting(catalogSetting, ""); err != nil {
		return err
	}
	return c.OK(200, "provider.catalog", s.catalogJSON())
}
