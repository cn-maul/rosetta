package rosetta

import "strings"

// Vendor is a built-in preset for a known provider: default endpoint,
// protocol and compatibility quirks. WithVendor("deepseek") fills in any
// endpoint/protocol the caller left blank; explicit options always win.
type Vendor struct {
	Name     string
	Protocol Protocol
	Endpoint string
	Quirks   Quirks
}

// builtinVendors is the preset table (cc-switch-inspired vendor presets).
// Endpoints include their version path so joinEndpoint appends only the
// resource path.
var builtinVendors = map[string]Vendor{
	"openai":      {Name: "openai", Protocol: ProtoOpenAIChat, Endpoint: "https://api.openai.com/v1"},
	"anthropic":   {Name: "anthropic", Protocol: ProtoAnthropic, Endpoint: "https://api.anthropic.com/v1"},
	"deepseek":    {Name: "deepseek", Protocol: ProtoOpenAIChat, Endpoint: "https://api.deepseek.com/v1"},
	"moonshot":    {Name: "moonshot", Protocol: ProtoOpenAIChat, Endpoint: "https://api.moonshot.cn/v1"},
	"qwen":        {Name: "qwen", Protocol: ProtoOpenAIChat, Endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1"},
	"zhipu":       {Name: "zhipu", Protocol: ProtoOpenAIChat, Endpoint: "https://open.bigmodel.cn/api/paas/v4"},
	"siliconflow": {Name: "siliconflow", Protocol: ProtoOpenAIChat, Endpoint: "https://api.siliconflow.cn/v1"},
	"openrouter":  {Name: "openrouter", Protocol: ProtoOpenAIChat, Endpoint: "https://openrouter.ai/api/v1"},
	"groq":        {Name: "groq", Protocol: ProtoOpenAIChat, Endpoint: "https://api.groq.com/openai/v1"},
	"together":    {Name: "together", Protocol: ProtoOpenAIChat, Endpoint: "https://api.together.xyz/v1"},
	"fireworks":   {Name: "fireworks", Protocol: ProtoOpenAIChat, Endpoint: "https://api.fireworks.ai/inference/v1"},
	"xai":         {Name: "xai", Protocol: ProtoOpenAIChat, Endpoint: "https://api.x.ai/v1"},
	"mistral":     {Name: "mistral", Protocol: ProtoOpenAIChat, Endpoint: "https://api.mistral.ai/v1"},
	"ollama":      {Name: "ollama", Protocol: ProtoOpenAIChat, Endpoint: "http://localhost:11434/v1"},
}

// VendorNames lists the built-in vendor presets (sorted keys not
// guaranteed; for discovery/UI purposes).
func VendorNames() []string {
	out := make([]string, 0, len(builtinVendors))
	for k := range builtinVendors {
		out = append(out, k)
	}
	return out
}

// normalizeVendor canonicalizes a vendor name.
func normalizeVendor(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
