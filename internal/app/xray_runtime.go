package app

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
	xrayruntime "github.com/perfect-panel/moment/xray-agent/internal/xray"
)

type runtimeLogConfig struct {
	AccessPath string
	ErrorPath  string
	Level      string
}

type runtimeAPIConfig struct {
	Enabled bool
	Port    string
}

func renderEffectiveRuntimeConfig(rawConfig []byte, users []serverclient.RuntimeUser, now time.Time, apiConfig runtimeAPIConfig, logConfig runtimeLogConfig) ([]byte, error) {
	withUsers, err := injectRuntimeUsers(rawConfig, users, now)
	if err != nil {
		return nil, err
	}
	withAPI, err := injectRuntimeAPI(withUsers, apiConfig)
	if err != nil {
		return nil, err
	}
	return injectRuntimeLog(withAPI, logConfig)
}

func injectRuntimeUsers(rawConfig []byte, users []serverclient.RuntimeUser, now time.Time) ([]byte, error) {
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawConfig, &object); err != nil || object == nil {
		return nil, errors.New("runtime xray config must be a json object")
	}
	if len(object["inbounds"]) == 0 {
		return json.Marshal(object)
	}
	var inbounds []json.RawMessage
	if err := json.Unmarshal(object["inbounds"], &inbounds); err != nil {
		return nil, err
	}
	activeUsers := activeRuntimeUsers(users, now)
	renderedInbounds := make([]json.RawMessage, 0, len(inbounds))
	for _, rawInbound := range inbounds {
		inbound, err := injectInboundRuntimeUsers(rawInbound, activeUsers)
		if err != nil {
			return nil, err
		}
		renderedInbounds = append(renderedInbounds, inbound)
	}
	rawInbounds, err := json.Marshal(renderedInbounds)
	if err != nil {
		return nil, err
	}
	object["inbounds"] = rawInbounds
	return json.Marshal(object)
}

func injectRuntimeLog(rawConfig []byte, logConfig runtimeLogConfig) ([]byte, error) {
	if logConfig.AccessPath == "" && logConfig.ErrorPath == "" && logConfig.Level == "" {
		return rawConfig, nil
	}
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawConfig, &object); err != nil || object == nil {
		return nil, errors.New("runtime xray config must be a json object")
	}
	logObject := map[string]json.RawMessage{}
	if len(object["log"]) > 0 {
		if err := json.Unmarshal(object["log"], &logObject); err != nil || logObject == nil {
			return nil, errors.New("runtime xray log config must be a json object")
		}
	}
	if logConfig.AccessPath != "" {
		setRawString(logObject, "access", logConfig.AccessPath)
	}
	if logConfig.ErrorPath != "" {
		setRawString(logObject, "error", logConfig.ErrorPath)
	}
	if logConfig.Level != "" {
		setRawString(logObject, "loglevel", normalizeXrayLogLevel(logConfig.Level))
	}
	rawLog, err := json.Marshal(logObject)
	if err != nil {
		return nil, err
	}
	object["log"] = rawLog
	return json.Marshal(object)
}

func injectRuntimeAPI(rawConfig []byte, apiConfig runtimeAPIConfig) ([]byte, error) {
	if !apiConfig.Enabled {
		return rawConfig, nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(apiConfig.Port))
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("xray api port is invalid")
	}
	var object map[string]any
	if err := json.Unmarshal(rawConfig, &object); err != nil || object == nil {
		return nil, errors.New("runtime xray config must be a json object")
	}
	object["stats"] = asRuntimeMap(object["stats"])
	object["api"] = map[string]any{
		"tag":      "api",
		"services": []any{"HandlerService", "StatsService", "RoutingService"},
	}
	object["policy"] = ensureRuntimePolicy(object["policy"])
	object["inbounds"] = ensureRuntimeAPIInbound(asRuntimeSlice(object["inbounds"]), port)
	object["routing"] = ensureRuntimeAPIRouting(asRuntimeMap(object["routing"]))
	return json.Marshal(object)
}

func ensureRuntimePolicy(value any) map[string]any {
	policy := asRuntimeMap(value)
	levels := asRuntimeMap(policy["levels"])
	level0 := asRuntimeMap(levels["0"])
	level0["statsUserUplink"] = true
	level0["statsUserDownlink"] = true
	levels["0"] = level0
	policy["levels"] = levels

	system := asRuntimeMap(policy["system"])
	system["statsInboundUplink"] = true
	system["statsInboundDownlink"] = true
	system["statsOutboundUplink"] = true
	system["statsOutboundDownlink"] = true
	policy["system"] = system
	return policy
}

func ensureRuntimeAPIInbound(inbounds []any, port int) []any {
	apiInbound := map[string]any{
		"tag":      "api",
		"listen":   "127.0.0.1",
		"port":     port,
		"protocol": "dokodemo-door",
		"settings": map[string]any{"address": "127.0.0.1"},
	}
	result := make([]any, 0, len(inbounds)+1)
	inserted := false
	for _, item := range inbounds {
		inbound := asRuntimeMap(item)
		if inbound["tag"] == "api" {
			if !inserted {
				result = append(result, apiInbound)
				inserted = true
			}
			continue
		}
		result = append(result, item)
	}
	if !inserted {
		result = append(result, apiInbound)
	}
	return result
}

func ensureRuntimeAPIRouting(routing map[string]any) map[string]any {
	rules := asRuntimeSlice(routing["rules"])
	next := make([]any, 0, len(rules)+1)
	next = append(next, map[string]any{
		"type":        "field",
		"inboundTag":  []any{"api"},
		"outboundTag": "api",
	})
	for _, item := range rules {
		rule := asRuntimeMap(item)
		if isRuntimeAPIRoute(rule) {
			continue
		}
		next = append(next, item)
	}
	routing["rules"] = next
	return routing
}

func isRuntimeAPIRoute(rule map[string]any) bool {
	if rule["outboundTag"] == "api" {
		return true
	}
	return runtimeValueContainsString(rule["inboundTag"], "api")
}

func runtimeValueContainsString(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []any:
		for _, item := range typed {
			if runtimeValueContainsString(item, expected) {
				return true
			}
		}
	case []string:
		for _, item := range typed {
			if item == expected {
				return true
			}
		}
	}
	return false
}

func asRuntimeMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func asRuntimeSlice(value any) []any {
	if value == nil {
		return []any{}
	}
	if typed, ok := value.([]any); ok {
		return typed
	}
	return []any{}
}

func normalizeXrayLogLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "warn":
		return "warning"
	case "debug", "info", "warning", "error", "none":
		return strings.ToLower(strings.TrimSpace(level))
	default:
		return "warning"
	}
}

func activeRuntimeUsers(users []serverclient.RuntimeUser, now time.Time) []serverclient.RuntimeUser {
	active := make([]serverclient.RuntimeUser, 0, len(users))
	for _, user := range users {
		if !user.Enabled || strings.TrimSpace(user.UUID) == "" || strings.TrimSpace(user.Password) == "" {
			continue
		}
		if user.ExpiredAtUnixMs > 0 && !time.UnixMilli(user.ExpiredAtUnixMs).After(now) {
			continue
		}
		active = append(active, user)
	}
	return active
}

func injectInboundRuntimeUsers(rawInbound json.RawMessage, users []serverclient.RuntimeUser) (json.RawMessage, error) {
	inbound := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawInbound, &inbound); err != nil || inbound == nil {
		return nil, errors.New("runtime xray inbound must be a json object")
	}
	protocol := strings.ToLower(strings.TrimSpace(jsonStringField(inbound, "protocol")))
	switch protocol {
	case "vless", "vmess", "trojan":
		return injectClientList(inbound, protocol, users)
	case "shadowsocks", "hysteria":
		return injectUserList(inbound, protocol, users)
	default:
		return rawInbound, nil
	}
}

func injectClientList(inbound map[string]json.RawMessage, protocol string, users []serverclient.RuntimeUser) (json.RawMessage, error) {
	settings, err := inboundSettings(inbound)
	if err != nil {
		return nil, err
	}
	template := firstObjectInArray(settings["clients"])
	clients := make([]map[string]json.RawMessage, 0, len(users))
	for _, user := range users {
		client := cloneRawObject(template)
		setRawString(client, "email", xrayruntime.RuntimeUserEmail(user.SubscriptionID))
		switch protocol {
		case "vless", "vmess":
			setRawString(client, "id", user.UUID)
		case "trojan":
			setRawString(client, "password", user.Password)
		}
		clients = append(clients, client)
	}
	rawClients, err := json.Marshal(clients)
	if err != nil {
		return nil, err
	}
	settings["clients"] = rawClients
	return marshalInboundSettings(inbound, settings)
}

func injectUserList(inbound map[string]json.RawMessage, protocol string, users []serverclient.RuntimeUser) (json.RawMessage, error) {
	settings, err := inboundSettings(inbound)
	if err != nil {
		return nil, err
	}
	template := firstObjectInArray(settings["users"])
	runtimeUsers := make([]map[string]json.RawMessage, 0, len(users))
	for _, user := range users {
		runtimeUser := cloneRawObject(template)
		setRawString(runtimeUser, "email", xrayruntime.RuntimeUserEmail(user.SubscriptionID))
		switch protocol {
		case "hysteria":
			setRawString(runtimeUser, "auth", user.Password)
		case "shadowsocks":
			setRawString(runtimeUser, "password", user.Password)
		}
		runtimeUsers = append(runtimeUsers, runtimeUser)
	}
	rawUsers, err := json.Marshal(runtimeUsers)
	if err != nil {
		return nil, err
	}
	settings["users"] = rawUsers
	return marshalInboundSettings(inbound, settings)
}

func inboundSettings(inbound map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	settings := map[string]json.RawMessage{}
	if len(inbound["settings"]) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(inbound["settings"], &settings); err != nil || settings == nil {
		return nil, errors.New("runtime xray inbound settings must be a json object")
	}
	return settings, nil
}

func marshalInboundSettings(inbound map[string]json.RawMessage, settings map[string]json.RawMessage) (json.RawMessage, error) {
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	inbound["settings"] = rawSettings
	return json.Marshal(inbound)
}

func firstObjectInArray(raw json.RawMessage) map[string]json.RawMessage {
	var values []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil || len(values) == 0 || values[0] == nil {
		return map[string]json.RawMessage{}
	}
	return values[0]
}

func cloneRawObject(object map[string]json.RawMessage) map[string]json.RawMessage {
	clone := make(map[string]json.RawMessage, len(object)+3)
	for key, value := range object {
		clone[key] = cloneRaw(value)
	}
	return clone
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func setRawString(object map[string]json.RawMessage, key string, value string) {
	raw, _ := json.Marshal(value)
	object[key] = raw
}

func jsonStringField(object map[string]json.RawMessage, key string) string {
	var value string
	if len(object[key]) == 0 || json.Unmarshal(object[key], &value) != nil {
		return ""
	}
	return value
}
