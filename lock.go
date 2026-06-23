package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
)

// ServiceCall is a Home Assistant service invocation.
type ServiceCall struct {
	Domain  string
	Service string
	Data    map[string]any
}

func (c ServiceCall) String() string {
	b, _ := json.Marshal(c.Data)
	return fmt.Sprintf("%s.%s %s", c.Domain, c.Service, b)
}

// CommandSpec is one lock command as data: the HA service to call and its data
// payload. Values may contain tokens substituted at call time:
//
//	__ENTITY__  the lock entity_id
//	__SLOT__    the code slot (emitted as a number when it is the whole value)
//	__CODE__    the guest's code (phone last 4)
type CommandSpec struct {
	Service string         `yaml:"service"` // "domain.service"
	Data    map[string]any `yaml:"data"`
}

func (c CommandSpec) build(entity string, slot int, code string) ServiceCall {
	domain, service, _ := strings.Cut(c.Service, ".")
	data := make(map[string]any, len(c.Data))
	for k, v := range c.Data {
		data[k] = substitute(v, entity, slot, code)
	}
	return ServiceCall{Domain: domain, Service: service, Data: data}
}

// substitute replaces tokens in a string value. A value that is exactly
// "__SLOT__" becomes an int; everything else is string replacement. Non-string
// values (numbers, bools) pass through untouched.
func substitute(v any, entity string, slot int, code string) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if s == "__SLOT__" {
		return slot
	}
	return strings.NewReplacer(
		"__ENTITY__", entity,
		"__CODE__", code,
		"__SLOT__", strconv.Itoa(slot),
	).Replace(s)
}

// DriverSpec is a lock model: its set/clear commands plus optional name hints
// used to guess the driver in `airlock -locks`.
type DriverSpec struct {
	Match []string    `yaml:"match"`
	Set   CommandSpec `yaml:"set"`
	Clear CommandSpec `yaml:"clear"`
}

func (d DriverSpec) SetCode(entity string, slot int, code string) ServiceCall {
	return d.Set.build(entity, slot, code)
}
func (d DriverSpec) ClearCode(entity string, slot int) ServiceCall {
	return d.Clear.build(entity, slot, "")
}

// Available reports whether the set command's service is registered in HA.
func (d DriverSpec) Available(svc Services) bool {
	domain, service, ok := strings.Cut(d.Set.Service, ".")
	return ok && svc.Has(domain, service)
}

func (d DriverSpec) MatchesEntity(e Entity) bool {
	return containsAny(e.EntityID+" "+e.FriendlyName(), d.Match...)
}

// Registry is the set of drivers available to a run.
type Registry map[string]DriverSpec

// BuiltinDrivers are the drivers airlock ships with. Config may override or
// extend them.
func BuiltinDrivers() map[string]DriverSpec {
	return map[string]DriverSpec{
		"zwave_js": {
			Match: []string{"z_wave", "zwave", "z-wave"},
			Set: CommandSpec{"zwave_js.set_lock_usercode", map[string]any{
				"entity_id": "__ENTITY__", "code_slot": "__SLOT__", "usercode": "__CODE__"}},
			Clear: CommandSpec{"zwave_js.clear_lock_usercode", map[string]any{
				"entity_id": "__ENTITY__", "code_slot": "__SLOT__"}},
		},
		"zha": {
			Match: []string{"zha", "zigbee"},
			Set: CommandSpec{"zha.set_lock_user_code", map[string]any{
				"entity_id": "__ENTITY__", "code_slot": "__SLOT__", "user_code": "__CODE__"}},
			Clear: CommandSpec{"zha.clear_lock_user_code", map[string]any{
				"entity_id": "__ENTITY__", "code_slot": "__SLOT__"}},
		},
	}
}

// BuildRegistry merges built-in drivers with config-defined ones (config wins).
func BuildRegistry(custom map[string]DriverSpec) Registry {
	reg := Registry{}
	maps.Copy(reg, BuiltinDrivers())
	maps.Copy(reg, custom)
	return reg
}

func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for n := range r {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Guess returns the available driver that claims the entity by name, or "" if
// none or more than one match.
func (r Registry) Guess(e Entity, available []string) string {
	var match string
	for _, name := range available {
		if r[name].MatchesEntity(e) {
			if match != "" {
				return "" // ambiguous
			}
			match = name
		}
	}
	return match
}

func containsAny(s string, subs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
