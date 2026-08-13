package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginIdentifier = "imagiflow"

var currentConfig atomic.Value

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// modelConfig describes one image model the plugin bridges to the standard
// /v1/images/generations interface. Each model can target its own gateway and
// customize how the generated image is extracted from the upstream response.
type modelConfig struct {
	Model       string `yaml:"model" json:"model"`
	BaseURL     string `yaml:"base_url" json:"base_url"`
	APIKey      string `yaml:"api_key" json:"api_key"`
	AuthIndex   string `yaml:"auth_index" json:"auth_index"`
	DefaultSize string `yaml:"default_size" json:"default_size"`
	// ImageFrom selects where to look for the generated image in the upstream
	// chat/completions response. Supported values:
	//   - "auto" (default): message.images[].image_url.url, then content image_url parts
	//   - "message_images": choices[0].message.images[].image_url.url only
	//   - "content_image": content array image_url parts only
	//   - "content_text": content array text parts, each treated as an image URL/data URL
	//   - "data": top-level data[] entries with url/b64_json
	ImageFrom string `yaml:"image_from" json:"image_from"`
	// ResponseFormat is the per-model default response format ("url" or
	// "b64_json") used when the client omits response_format.
	ResponseFormat string `yaml:"response_format" json:"response_format"`
}

// pluginConfig holds the runtime configuration for this plugin.
//
// The plugin converts the standard OpenAI image generation interface
// (/v1/images/generations) into a chat/completions request toward a gateway
// that only exposes the configured image model(s) through /v1/chat/completions,
// and converts the image-bearing response back into the standard image API shape.
//
// `models` is the preferred multi-model configuration. The legacy single-model
// fields (`model`, `base_url`, `api_key`, `default_size`) are also honored and
// are always appended as one extra model entry when non-empty.
type pluginConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Model          string        `yaml:"model"`
	BaseURL        string        `yaml:"base_url"`
	APIKey         string        `yaml:"api_key"`
	AuthIndex      string        `yaml:"auth_index"`
	DefaultSize    string        `yaml:"default_size"`
	ImageFrom      string        `yaml:"image_from"`
	ResponseFormat string        `yaml:"response_format"`
	Models         []modelConfig `yaml:"models"`
	TimeoutSeconds int           `yaml:"timeout_seconds"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}

// managementResourceRoute describes a browser-navigable management page.
type managementResourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

type managementRegisterResult struct {
	Resources []managementResourceRoute `json:"resources"`
}

type managementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return errorEnvelope("not_supported", "streaming is not supported for image generation"), nil
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		Model:          "gemini-3.1-flash-image",
		BaseURL:        "http://8.137.16.163:8317/v1",
		DefaultSize:    "1024x1024",
		TimeoutSeconds: 90,
	}
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.AuthIndex = strings.TrimSpace(cfg.AuthIndex)
	cfg.DefaultSize = strings.TrimSpace(cfg.DefaultSize)
	cfg.ImageFrom = strings.TrimSpace(cfg.ImageFrom)
	cfg.ResponseFormat = normalizeResponseFormat(strings.TrimSpace(cfg.ResponseFormat))
	cfg.TimeoutSeconds = normalizeTimeout(cfg.TimeoutSeconds)

	// normalize each multi-model entry
	models := make([]modelConfig, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		m.Model = strings.TrimSpace(m.Model)
		m.BaseURL = strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
		m.APIKey = strings.TrimSpace(m.APIKey)
		m.AuthIndex = strings.TrimSpace(m.AuthIndex)
		m.DefaultSize = strings.TrimSpace(m.DefaultSize)
		m.ImageFrom = strings.TrimSpace(m.ImageFrom)
		m.ResponseFormat = normalizeResponseFormat(strings.TrimSpace(m.ResponseFormat))
		if m.Model == "" || (m.BaseURL == "" && m.AuthIndex == "") {
			continue
		}
		models = append(models, m)
	}
	cfg.Models = models

	// When the multi-model list is provided, suppress the legacy single-model
	// fields so they do not inject an extra default entry with an empty api key.
	if len(models) > 0 {
		cfg.Model = ""
		cfg.BaseURL = ""
		cfg.APIKey = ""
		cfg.AuthIndex = ""
		cfg.DefaultSize = ""
		cfg.ImageFrom = ""
		cfg.ResponseFormat = ""
	}
	return cfg, nil
}

func normalizeResponseFormat(v string) string {
	f := strings.ToLower(strings.TrimSpace(v))
	if f == "url" || f == "b64_json" {
		return f
	}
	return ""
}

func normalizeTimeout(v int) int {
	if v <= 0 {
		return 90
	}
	return v
}

// resolvedModels returns the effective list of image models a client may pick.
// It always includes the legacy single-model entry when configured, followed by
// every entry in `models`. Duplicate names are collapsed (first wins).
func resolvedModels(cfg pluginConfig) []modelConfig {
	var out []modelConfig
	seen := map[string]bool{}
	if m, ok := legacyModelEntry(cfg); ok && !seen[m.Model] {
		seen[m.Model] = true
		out = append(out, m)
	}
	for _, m := range cfg.Models {
		if seen[m.Model] {
			continue
		}
		seen[m.Model] = true
		out = append(out, m)
	}
	return out
}

func legacyModelEntry(cfg pluginConfig) (modelConfig, bool) {
	model := cfg.Model
	apiKey := cfg.APIKey
	authIndex := cfg.AuthIndex
	if model == "" && apiKey == "" && authIndex == "" {
		return modelConfig{}, false
	}
	if model == "" {
		model = defaultPluginConfig().Model
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultPluginConfig().BaseURL
	}
	return modelConfig{
		Model:          model,
		BaseURL:        baseURL,
		APIKey:         apiKey,
		AuthIndex:      authIndex,
		DefaultSize:    cfg.DefaultSize,
		ImageFrom:      cfg.ImageFrom,
		ResponseFormat: cfg.ResponseFormat,
	}, true
}

// modelFor returns the matching configured model entry for a requested model.
func modelFor(cfg pluginConfig, requested string) (modelConfig, bool) {
	for _, m := range resolvedModels(cfg) {
		if strings.EqualFold(strings.TrimSpace(m.Model), strings.TrimSpace(requested)) {
			return m, true
		}
	}
	return modelConfig{}, false
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "imagiflow",
			Version:          "0.1.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When false, the plugin declines all image generation requests."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "List of image models to expose on /v1/images/generations. Each entry: model, base_url, api_key, auth_index, default_size, image_from, response_format. Omit base_url/api_key and set auth_index to resolve the upstream interface from an existing CPA credential at runtime."},
				{Name: "model", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy single-model name (classic/{base_url,api_key,auth_index,default_size}). Optional when `models` is used."},
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy gateway base URL that accepts /v1/chat/completions."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy gateway API key sent as Authorization: Bearer."},
				{Name: "auth_index", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy credential auth_index used to resolve base_url/api_key at runtime when base_url/api_key are empty."},
				{Name: "default_size", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy default image size when the client omits size."},
				{Name: "image_from", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"", "auto", "message_images", "content_image", "content_text", "data"}, Description: "Where to extract the generated image from the upstream chat/completions response. Global default for all models; override per model inside `models`. Empty = auto."},
				{Name: "response_format", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"", "url", "b64_json"}, Description: "Default response format (url or b64_json) when the client omits response_format. Global default for all models; override per model inside `models`. Empty = default behavior."},
				{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Upstream chat/completions timeout in seconds (default 90)."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			ManagementAPI:         true,
		},
	}
}

// managementRegistration declares the browser-navigable management page that
// lets operators configure the bridge models (name, endpoint, response) from
// the CPA web UI instead of editing YAML directly.
func managementRegistration() managementRegisterResult {
	return managementRegisterResult{
		Resources: []managementResourceRoute{{
			Path:        "/models",
			Menu:        "图像模型管理",
			Description: "管理本插件桥接的图像模型:模型名 / 网关接口(地址与密钥) / 图片响应位置与格式。",
		}},
	}
}

// handleManagement serves the management resource page (GET). The page talks
// to the host Management API directly to read and persist the plugin config,
// so no state lives in the plugin beyond the normal reconfigure flow.
func handleManagement(raw []byte) ([]byte, error) {
	var req struct {
		Method string `json:"Method"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if strings.EqualFold(req.Method, http.MethodGet) {
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       []byte(managementPageHTML()),
		})
	}
	return okEnvelope(managementResponse{StatusCode: http.StatusNotFound})
}

// managementPageHTML returns the standalone management page. It reads the
// current plugin config from the host and lets the user edit the bridge models
// as a table (model / base_url / api_key / default_size / image_from /
// response_format), saving back through the host config endpoint.
func managementPageHTML() string {
	return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ImagiFlow 图像模型管理</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { font-family: system-ui, sans-serif; margin: 24px; line-height: 1.5; }
  h1 { font-size: 20px; }
  p.hint { color: #888; font-size: 13px; margin-top: -6px; }
  table { border-collapse: collapse; width: 100%; margin-top: 12px; }
  th, td { border: 1px solid #444; padding: 6px 8px; text-align: left; font-size: 13px; }
  input, select { width: 100%; padding: 4px 6px; }
  .row-actions { white-space: nowrap; text-align: center; }
  .btn { margin-top: 12px; padding: 8px 18px; cursor: pointer; }
  .add { margin-top: 10px; }
  .picker { margin-top: 12px; padding: 10px; border: 1px dashed #555; border-radius: 6px; }
  .picker select { width: auto; min-width: 260px; }
  #modelList { margin-top: 8px; }
  .mod-chip { display: inline-block; margin: 4px 6px 0 0; padding: 4px 10px; border: 1px solid #555; border-radius: 14px; cursor: pointer; font-size: 12px; }
  .mod-chip:hover { background: #333; }
  .status { margin-top: 12px; font-weight: 600; }
  .status.err { color: #e5484d; }
  .status.ok { color: #30a46c; }
  .remove { cursor: pointer; color: #e5484d; background: none; border: none; font-size: 15px; }
</style>
</head>
<body>
  <h1>ImagiFlow 图像模型管理</h1>
  <p class="hint">列出通过本插件桥接到 /v1/images/generations 的图像模型。改动保存后即时生效并持久化到配置。</p>
  <div class="picker">
    <select id="authSel"><option value="">(选择已有认证文件)</option></select>
    <button class="btn add" type="button" onclick="loadModels()">加载该认证的模型</button>
    <span id="modelsHint" class="hint"></span>
    <div id="modelList"></div>
  </div>
  <table>
    <thead>
      <tr>
        <th style="width:13%">模型名</th>
        <th style="width:15%">认证 auth_index</th>
        <th style="width:22%">网关地址 base_url</th>
        <th style="width:17%">api_key</th>
        <th style="width:9%">默认尺寸</th>
        <th style="width:10%">image_from</th>
        <th style="width:7%">response_format</th>
        <th style="width:4%"></th>
      </tr>
    </thead>
    <tbody id="rows"></tbody>
  </table>
  <button class="btn add" type="button" onclick="addRow()">+ 添加模型</button>
  <div><button class="btn" type="button" onclick="save()">保存</button></div>
  <div class="status" id="status"></div>

<script>
var cfg = { enabled: true, models: [] };
var IMAGE_FROM = ['', 'auto', 'message_images', 'content_image', 'content_text', 'data'];
var RESP_FORMAT = ['', 'url', 'b64_json'];

function esc(v) {
  return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}
function opts(values, cur) {
  return values.map(function (v) {
    return '<option value="' + esc(v) + '"' + (String(v) === String(cur) ? ' selected' : '') + '>' + (v === '' ? '(默认)' : esc(v)) + '</option>';
  }).join('');
}
function rowHtml(m, i) {
  m = m || {};
  return '<tr>'
    + '<td><input data-i="' + i + '" data-f="model" value="' + esc(m.model) + '" placeholder="gemini-3.1-flash-image"></td>'
    + '<td><input data-i="' + i + '" data-f="auth_index" value="' + esc(m.auth_index) + '" placeholder="已有认证 auth_index"></td>'
    + '<td><input data-i="' + i + '" data-f="base_url" value="' + esc(m.base_url) + '" placeholder="https://.../v1 (留空用认证)"></td>'
    + '<td><input data-i="' + i + '" data-f="api_key" value="' + esc(m.api_key) + '" placeholder="密钥 (留空用认证)"></td>'
    + '<td><input data-i="' + i + '" data-f="default_size" value="' + esc(m.default_size) + '" placeholder="1024x1024"></td>'
    + '<td><select data-i="' + i + '" data-f="image_from">' + opts(IMAGE_FROM, m.image_from) + '</select></td>'
    + '<td><select data-i="' + i + '" data-f="response_format">' + opts(RESP_FORMAT, m.response_format) + '</select></td>'
    + '<td class="row-actions"><button type="button" class="remove" onclick="removeRow(' + i + ')">\u2715</button></td>'
    + '</tr>';
}
function render() {
  var rows = Array.isArray(cfg.models) ? cfg.models : [];
  var tbody = document.getElementById('rows');
  tbody.innerHTML = rows.map(rowHtml).join('');
}
function addFromAuth(authIndex, model) {
  if (!Array.isArray(cfg.models)) cfg.models = [];
  cfg.models.push({ auth_index: authIndex, model: model, image_from: 'auto' });
  render();
}
function addRow() {
  if (!Array.isArray(cfg.models)) cfg.models = [];
  cfg.models.push({ image_from: 'auto' });
  render();
}
function loadModels() {
  var sel = document.getElementById('authSel');
  var name = sel.value;
  var authIndex = sel.options[sel.selectedIndex] ? sel.options[sel.selectedIndex].getAttribute('data-auth') || '' : '';
  var list = document.getElementById('modelList');
  var hint = document.getElementById('modelsHint');
  if (!name) { list.innerHTML = ''; hint.textContent = '请先选择认证文件'; return; }
  hint.textContent = '加载中...';
  fetch('/v0/management/auth-files/models?name=' + encodeURIComponent(name))
    .then(function (r) { return r.json(); })
    .then(function (j) {
      var ms = (j && j.models) || [];
      hint.textContent = ms.length ? ('点击模型名加入(' + name + ')') : ('该认证没有可用的模型列表(' + name + ')');
      list.innerHTML = ms.map(function (m) {
        return '<span class="mod-chip" onclick="addFromAuth(\'' + esc(authIndex) + '\',\'' + esc(m.id) + '\')">' + esc(m.id) + '</span>';
      }).join('');
    })
    .catch(function (e) { hint.textContent = '加载失败: ' + e.message; });
}
function removeRow(i) {
  cfg.models.splice(i, 1);
  render();
}
function setStatus(text, err) {
  var el = document.getElementById('status');
  el.textContent = text;
  el.className = 'status ' + (err ? 'err' : 'ok');
}
function save() {
  var tbody = document.getElementById('rows');
  var models = [];
  tbody.querySelectorAll('tr').forEach(function (tr) {
    var m = {};
    tr.querySelectorAll('input,select').forEach(function (el) {
      m[el.getAttribute('data-f')] = el.value.trim();
    });
    if (m.model || m.base_url || m.api_key || m.auth_index) models.push(m);
  });
  cfg.models = models;
  var url = '/v0/management/plugins/' + encodeURIComponent('imagiflow') + '/config';
  setStatus('保存中...');
  fetch(url, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(cfg) })
    .then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; }); })
    .then(function (res) {
      if (res.ok) { setStatus('已保存'); }
      else { setStatus('保存失败: ' + (res.j.message || res.j.error || '未知错误'), true); }
    })
    .catch(function (e) { setStatus('保存失败: ' + e.message, true); });
}
fetch('/v0/management/plugins/' + encodeURIComponent('imagiflow') + '/config')
  .then(function (r) { return r.json(); })
  .then(function (j) { cfg = j || cfg; render(); })
  .catch(function (e) { setStatus('加载配置失败: ' + e.message, true); });
fetch('/v0/management/auth-files')
  .then(function (r) { return r.json(); })
  .then(function (j) {
    var files = (j && j.files) || [];
    var sel = document.getElementById('authSel');
    sel.innerHTML = '<option value="">(选择已有认证文件)</option>' + files.map(function (f) {
      return '<option value="' + esc(f.name) + '" data-auth="' + esc(f.auth_index) + '">' + esc(f.name) + (f.type ? ' [' + esc(f.type) + ']' : '') + '</option>';
    }).join('');
  })
  .catch(function () { /* 认证列表不可用时仍可手动填写 */ });
</script>
</body>
</html>`
}

// routeModel decides whether this plugin handles a request for the configured
// image model. We claim the model only when (a) it is enabled, (b) the client
// model matches the configured image model, and (c) the payload looks like an
// image generation request (has a "prompt" and no ordinary text chat message).
// Anything else is declined so normal chat traffic keeps flowing.
func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if len(resolvedModels(cfg)) == 0 {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if _, ok := modelFor(cfg, req.RequestedModel); !ok {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if !looksLikeImageGeneration(req.Body) && !isImagesSourceFormat(req.SourceFormat) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "gemini_flash_image_generation",
	})
}

func isImagesSourceFormat(format string) bool {
	f := strings.ToLower(strings.TrimSpace(format))
	return f == "images" || strings.Contains(f, "image")
}

// looksLikeImageGeneration detects the standard /v1/images/generations body
// (a top-level "prompt") versus a chat conversation (messages array).
func looksLikeImageGeneration(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if !json.Valid(body) {
		return false
	}
	prompt := getJSONString(body, "prompt")
	return strings.TrimSpace(prompt) != ""
}

func getJSONString(body []byte, key string) string {
	var m map[string]any
	if errUnmarshal := json.Unmarshal(body, &m); errUnmarshal != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	body, errRun := runImageGeneration(context.Background(), req.ExecutorRequest, loadedConfig())
	if errRun != nil {
		return errorEnvelope("executor_error", errRun.Error()), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// ---------------------------------------------------------------------------
// Image generation conversion
// ---------------------------------------------------------------------------

// incomingImages mirrors the standard OpenAI image request. Besides the core
// fields it also carries optional reference/edit inputs so a single
// /v1/images/generations call can include a source picture:
//   - image: data URLs or bare base64 payloads to condition/edit on
//   - image_url: remote image URLs to condition/edit on
//
// These are forwarded to the upstream chat/completions request as image_url
// content parts, matching the README's documented behavior.
type incomingImages struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	Size           string   `json:"size"`
	ResponseFormat string   `json:"response_format"`
	N              int      `json:"n"`
	Image          []string `json:"image"`
	ImageURL       []string `json:"image_url"`
}

// chatContentPart is one part of the chat/completions user content.
type chatContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *chatImagePart `json:"image_url,omitempty"`
}

type chatImagePart struct {
	URL string `json:"url"`
}

type chatMessage struct {
	Role    string            `json:"role"`
	Content []chatContentPart `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Size     string        `json:"size,omitempty"`
}

// imagesAPIData is one entry of the standard image generation response.
type imagesAPIData struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

type imagesAPIResponse struct {
	Created int64           `json:"created"`
	Data    []imagesAPIData `json:"data"`
}

type upstreamImage struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// referenceImages resolves the client-supplied reference/edit images into a
// list of usable image URL values. `image` entries may be data URLs, bare
// base64 payloads, or remote URLs; `image_url` entries are expected to already
// be full URLs. Bare base64 payloads are wrapped into PNG data URLs.
func referenceImages(in incomingImages) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if !strings.HasPrefix(v, "data:") && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			v = "data:image/png;base64," + v
		}
		out = append(out, v)
	}
	for _, v := range in.ImageURL {
		add(v)
	}
	for _, v := range in.Image {
		add(v)
	}
	return out
}

func runImageGeneration(ctx context.Context, req pluginapi.ExecutorRequest, cfg pluginConfig) ([]byte, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("plugin disabled")
	}

	var in incomingImages
	source := req.OriginalRequest
	if len(bytes.TrimSpace(source)) == 0 {
		source = req.Payload
	}
	if errUnmarshal := json.Unmarshal(source, &in); errUnmarshal != nil {
		return nil, fmt.Errorf("decode images request: %w", errUnmarshal)
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	// Resolve the configured entry for the client-requested model so each model
	// can target its own gateway/api key/size.
	reqModel := strings.TrimSpace(in.Model)
	if reqModel == "" {
		reqModel = req.Model
	}
	mc, ok := modelFor(cfg, reqModel)
	if !ok {
		return nil, fmt.Errorf("model %q is not configured", reqModel)
	}
	if resolved, errResolve := resolveModelEndpoint(mc); errResolve != nil {
		return nil, errResolve
	} else {
		mc = resolved
	}

	defaultSize := mc.DefaultSize
	if defaultSize == "" {
		defaultSize = cfg.DefaultSize
	}
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = defaultSize
	}

	// Build the chat content: the text prompt, plus any reference/edit images
	// supplied by the client so they reach the upstream image model.
	content := []chatContentPart{{Type: "text", Text: prompt}}
	for _, ref := range referenceImages(in) {
		content = append(content, chatContentPart{Type: "image_url", ImageURL: &chatImagePart{URL: ref}})
	}

	reqBody, errMarshal := json.Marshal(chatRequest{
		Model:    mc.Model,
		Messages: []chatMessage{{Role: "user", Content: content}},
		Size:     size,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}

	upstream, errCall := callChatCompletions(ctx, mc, cfg.TimeoutSeconds, reqBody)
	if errCall != nil {
		return nil, errCall
	}
	imageFrom := mc.ImageFrom
	if imageFrom == "" {
		imageFrom = cfg.ImageFrom
	}
	responseFormat := strings.TrimSpace(in.ResponseFormat)
	if responseFormat == "" {
		responseFormat = mc.ResponseFormat
	}
	if responseFormat == "" {
		responseFormat = cfg.ResponseFormat
	}
	return buildImagesAPIResponse(upstream, responseFormat, imageFromMode(imageFrom))
}

func callChatCompletions(ctx context.Context, mc modelConfig, timeoutSeconds int, body []byte) ([]byte, error) {
	url := mc.BaseURL + "/chat/completions"
	timeout := time.Duration(normalizeTimeout(timeoutSeconds)) * time.Second
	httpClient := &http.Client{Timeout: timeout}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errNew != nil {
		return nil, errNew
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mc.APIKey)

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream http %d: %.400s", resp.StatusCode, string(raw))
	}
	return raw, nil
}

// imageFromMode normalizes a configured image extraction mode to a known value,
// defaulting to "auto" for empty or unknown modes.
func imageFromMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "message_images", "content_image", "content_text", "data":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "auto"
	}
}

// callHost invokes a plugin-host RPC method and returns the envelope result bytes.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	var resp C.cliproxy_buffer
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &resp) != 0 {
		return nil, fmt.Errorf("host call %s failed", method)
	}
	if resp.ptr == nil {
		return nil, fmt.Errorf("host call %s returned no data", method)
	}
	defer C.free_host_buffer(resp.ptr, resp.len)
	return C.GoBytes(resp.ptr, C.int(resp.len)), nil
}

// resolveModelEndpoint fills base_url/api_key for a configured model. If the
// entry has explicit base_url/api_key those win; otherwise, when auth_index is
// set, the plugin pulls the credential from the host (host.auth.get) and reads
// base_url/api_key from the auth JSON at runtime. This lets the plugin route to
// an existing CPA model's interface without storing upstream secrets in config.
func resolveModelEndpoint(mc modelConfig) (modelConfig, error) {
	baseURL := strings.TrimSpace(mc.BaseURL)
	apiKey := strings.TrimSpace(mc.APIKey)
	if baseURL != "" && apiKey != "" {
		return mc, nil
	}
	authIndex := strings.TrimSpace(mc.AuthIndex)
	if authIndex == "" {
		if baseURL == "" {
			return modelConfig{}, fmt.Errorf("base_url is not configured for model %q", mc.Model)
		}
		return modelConfig{}, fmt.Errorf("api_key is not configured for model %q", mc.Model)
	}
	raw, errCall := callHost(pluginabi.MethodHostAuthGet, mustJSON(pluginapi.HostAuthGetRequest{AuthIndex: authIndex}))
	if errCall != nil {
		return modelConfig{}, fmt.Errorf("resolve auth %q: %w", authIndex, errCall)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return modelConfig{}, fmt.Errorf("decode host auth response: %w", errUnmarshal)
	}
	if !env.OK || env.Error != nil {
		msg := "unknown error"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return modelConfig{}, fmt.Errorf("resolve auth %q: %s", authIndex, msg)
	}
	var got struct {
		JSON map[string]any `json:"json"`
	}
	if errUnmarshal := json.Unmarshal(env.Result, &got); errUnmarshal != nil {
		return modelConfig{}, fmt.Errorf("decode host auth result: %w", errUnmarshal)
	}
	baseURL = strings.TrimSpace(jsonString(got.JSON, "base_url"))
	apiKey = strings.TrimSpace(jsonString(got.JSON, "api_key"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(jsonString(got.JSON, "apiKey"))
	}
	if baseURL == "" {
		return modelConfig{}, fmt.Errorf("auth %q has no base_url", authIndex)
	}
	if apiKey == "" {
		return modelConfig{}, fmt.Errorf("auth %q has no api_key", authIndex)
	}
	mc.BaseURL = strings.TrimRight(baseURL, "/")
	mc.APIKey = apiKey
	return mc, nil
}

func jsonString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func mustJSON(v any) []byte {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return []byte("{}")
	}
	return raw
}

// buildImagesAPIResponse parses a chat/completions response that carries the
// generated image(s) and reshapes them into the standard images API response.
//
// The default gateway we bridge to returns images under:
//
//	choices[0].message.images[].image_url.url   (data:image/...;base64,...)
//
// If instead the images are in the normal chat content array, we handle that
// shape too. The `mode` argument lets per-model config override where images
// are extracted from, since different upstreams return them differently.
func buildImagesAPIResponse(upstream []byte, responseFormat, mode string) ([]byte, error) {
	var parsed chatCompletionResponse
	if errUnmarshal := json.Unmarshal(upstream, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("decode upstream response: %w", errUnmarshal)
	}
	images := collectImages(&parsed, mode)
	if len(images) == 0 {
		return nil, fmt.Errorf("upstream returned no images")
	}

	created := time.Now().Unix()
	data := make([]imagesAPIData, 0, len(images))
	for _, img := range images {
		b64, mime := splitDataURL(img)
		entry := imagesAPIData{}
		if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
			entry.URL = img
		} else if b64 != "" {
			entry.B64JSON = b64
		} else {
			entry.URL = img
		}
		if entry.B64JSON == "" && entry.URL == "" {
			continue
		}
		_ = mime
		data = append(data, entry)
	}
	if len(data) == 0 {
		// Fall back to returning the data URL directly.
		for _, img := range images {
			data = append(data, imagesAPIData{URL: img})
		}
	}
	return json.Marshal(imagesAPIResponse{Created: created, Data: data})
}

type chatCompletionResponse struct {
	Data    []imagesAPIData `json:"data"`
	Choices []struct {
		Message struct {
			Images  []upstreamImage `json:"images"`
			Content []upstreamPart  `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// upstreamPart is one element of the chat content array.
type upstreamPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *upstreamImageURL `json:"image_url,omitempty"`
}

type upstreamImageURL struct {
	URL string `json:"url"`
}

func collectImages(resp *chatCompletionResponse, mode string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	switch mode {
	case "message_images":
		for _, choice := range resp.Choices {
			for _, img := range choice.Message.Images {
				add(img.ImageURL.URL)
			}
		}
	case "content_image":
		for _, choice := range resp.Choices {
			for _, p := range choice.Message.Content {
				if p.ImageURL != nil {
					add(p.ImageURL.URL)
				}
			}
		}
	case "content_text":
		for _, choice := range resp.Choices {
			for _, p := range choice.Message.Content {
				add(p.Text)
			}
		}
	case "data":
		for _, d := range resp.Data {
			add(d.URL)
			add(d.B64JSON)
		}
	default: // auto
		for _, choice := range resp.Choices {
			for _, img := range choice.Message.Images {
				add(img.ImageURL.URL)
			}
			// chat content array may also carry image_url parts with data URLs
			for _, p := range choice.Message.Content {
				if p.ImageURL != nil {
					u := strings.TrimSpace(p.ImageURL.URL)
					if strings.HasPrefix(u, "data:image") {
						add(u)
					}
				}
			}
		}
	}
	return out
}

// splitDataURL breaks a data: URL into its base64 payload and mime type.
// Returns empty base64 when the value is not a data URL.
func splitDataURL(dataURL string) (string, string) {
	s := strings.TrimSpace(dataURL)
	if !strings.HasPrefix(s, "data:") {
		return "", ""
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return "", ""
	}
	header := s[5:comma]
	b64 := s[comma+1:]
	if strings.Contains(header, ";base64") {
		mime := strings.SplitN(header, ";", 2)[0]
		// validate base64
		if _, errDecode := base64.StdEncoding.DecodeString(b64); errDecode == nil {
			return b64, mime
		}
	}
	return "", ""
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
