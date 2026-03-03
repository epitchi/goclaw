package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/pkg/providers"
)

// --- Context helpers for media images ---

const ctxMediaImages toolContextKey = "tool_media_images"

// WithMediaImages stores base64-encoded images in context for read_image tool access.
func WithMediaImages(ctx context.Context, images []providers.ImageContent) context.Context {
	return context.WithValue(ctx, ctxMediaImages, images)
}

// MediaImagesFromCtx retrieves stored images from context.
func MediaImagesFromCtx(ctx context.Context) []providers.ImageContent {
	v, _ := ctx.Value(ctxMediaImages).([]providers.ImageContent)
	return v
}

// --- ReadImageTool ---

// visionProviderPriority is the order in which providers are tried for vision.
var visionProviderPriority = []string{"openrouter", "gemini", "anthropic", "dashscope"}

// visionModelOverrides maps provider names to preferred vision models.
// Providers not listed here use their default model.
var visionModelOverrides = map[string]string{
	"openrouter": "google/gemini-2.5-flash-image",
	"dashscope":  "qwen3-vl",
}

// ReadImageTool uses a vision-capable provider to describe images attached to the current message.
type ReadImageTool struct {
	registry *providers.Registry
}

func NewReadImageTool(registry *providers.Registry) *ReadImageTool {
	return &ReadImageTool{registry: registry}
}

func (t *ReadImageTool) Name() string { return "read_image" }

func (t *ReadImageTool) Description() string {
	return "Analyze images attached to the current message using a vision model. Use this when you see <media:image> tags but cannot view images directly."
}

func (t *ReadImageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"prompt": map[string]interface{}{
				"type":        "string",
				"description": "What you want to know about the image(s). E.g. 'Describe this image in detail' or 'What text is in this image?'",
			},
		},
		"required": []string{"prompt"},
	}
}

func (t *ReadImageTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		prompt = "Describe this image in detail."
	}

	images := MediaImagesFromCtx(ctx)
	if len(images) == 0 {
		return ErrorResult("No images available in this conversation. The user may not have sent an image.")
	}

	// Find a vision-capable provider (per-agent config > hardcoded priority)
	provider, model, err := t.resolveVisionProviderWithConfig(ctx)
	if err != nil {
		return ErrorResult(err.Error())
	}

	slog.Info("read_image: calling vision provider", "provider", provider.Name(), "model", model, "images", len(images))

	resp, err := provider.Chat(ctx, providers.ChatRequest{
		Messages: []providers.Message{
			{
				Role:    "user",
				Content: prompt,
				Images:  images,
			},
		},
		Model: model,
		Options: map[string]interface{}{
			"max_tokens":  1024,
			"temperature": 0.3,
		},
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("Vision provider error: %v", err))
	}

	result := NewResult(resp.Content)
	result.Usage = resp.Usage
	result.Provider = provider.Name()
	result.Model = model
	return result
}

// resolveVisionProviderWithConfig checks per-agent VisionConfig first,
// then global builtin_tools.settings, then falls back to hardcoded priority.
func (t *ReadImageTool) resolveVisionProviderWithConfig(ctx context.Context) (providers.Provider, string, error) {
	// 1. Per-agent override (highest priority)
	if cfg := VisionConfigFromCtx(ctx); cfg != nil && cfg.Provider != "" {
		p, err := t.registry.Get(cfg.Provider)
		if err != nil {
			return nil, "", fmt.Errorf("configured vision provider %q not available: %w", cfg.Provider, err)
		}
		model := cfg.Model
		if model == "" {
			model = p.DefaultModel()
		}
		return p, model, nil
	}
	// 2. Global builtin_tools.settings (DB defaults)
	if p, model, ok := t.resolveFromBuiltinSettings(ctx); ok {
		return p, model, nil
	}
	// 3. Hardcoded defaults
	return t.resolveVisionProvider()
}

// resolveFromBuiltinSettings checks global builtin tool settings for provider/model config.
func (t *ReadImageTool) resolveFromBuiltinSettings(ctx context.Context) (providers.Provider, string, bool) {
	settings := BuiltinToolSettingsFromCtx(ctx)
	if settings == nil {
		return nil, "", false
	}
	raw, ok := settings["read_image"]
	if !ok || len(raw) == 0 {
		return nil, "", false
	}
	var cfg struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Provider == "" {
		return nil, "", false
	}
	p, err := t.registry.Get(cfg.Provider)
	if err != nil {
		return nil, "", false
	}
	model := cfg.Model
	if model == "" {
		model = p.DefaultModel()
	}
	return p, model, true
}

// resolveVisionProvider finds the first available vision-capable provider.
func (t *ReadImageTool) resolveVisionProvider() (providers.Provider, string, error) {
	for _, name := range visionProviderPriority {
		p, err := t.registry.Get(name)
		if err != nil {
			continue
		}
		model := p.DefaultModel()
		if override, ok := visionModelOverrides[name]; ok {
			model = override
		}
		return p, model, nil
	}
	return nil, "", fmt.Errorf("no vision-capable provider available (need one of: %v)", visionProviderPriority)
}
