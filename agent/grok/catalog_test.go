package grok

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acpInitializeFrame is a trimmed capture of the real `grok agent stdio`
// initialize result (Grok Build 1.0.70). The handshake needs no login, which is
// why it is the adapter's primary catalog probe.
const acpInitializeFrame = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"authMethods":[{"id":"grok.com","name":"Grok","description":"Sign in with Grok"}],"_meta":{"modelState":{"currentModelId":"grok-4.6","availableModels":[{"modelId":"grok-4.6","name":"Grok 4.6","description":"SpaceXAI's latest frontier model","_meta":{"totalContextTokens":500000,"agentType":"grok-build-plan","supportsReasoningEffort":true,"reasoningEffort":"high","reasoningEfforts":[{"id":"xhigh","value":"xhigh","label":"Extra High Effort","default":false},{"id":"high","value":"high","label":"High Effort","default":true},{"id":"medium","value":"medium","label":"Medium Effort","default":false},{"id":"low","value":"low","label":"Low Effort","default":false}]}},{"modelId":"grok-4.5","name":"Grok 4.5","_meta":{"totalContextTokens":500000,"supportsReasoningEffort":true,"reasoningEffort":"high","reasoningEfforts":[{"id":"high","value":"high","default":true},{"id":"medium","value":"medium","default":false},{"id":"low","value":"low","default":false}]}}]}}}}`

func TestModelsFromACPFrame(t *testing.T) {
	models, defaultID := modelsFromACPFrame([]byte(acpInitializeFrame))
	require.Len(t, models, 2)
	assert.Equal(t, "grok-4.6", defaultID)

	assert.Equal(t, "grok-4.6", models[0].ID)
	assert.Equal(t, "Grok 4.6", models[0].Name)
	assert.Equal(t, 500000, models[0].ContextTokens)
	assert.Equal(t, "high", models[0].DefaultEffort)
	assert.Equal(t, []string{"xhigh", "high", "medium", "low"}, models[0].Efforts)

	assert.Equal(t, "grok-4.5", models[1].ID)
	assert.Equal(t, []string{"high", "medium", "low"}, models[1].Efforts)
	// grok-4.5 carries no description; the default marker must not leak onto it.
	assert.Empty(t, models[1].Description)
}

func TestModelsFromACPFrame_NonResultFrames(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{}}`,
		`not json`,
		`{"result":{}}`,
	} {
		models, defaultID := modelsFromACPFrame([]byte(frame))
		assert.Nil(t, models, frame)
		assert.Empty(t, defaultID, frame)
	}
}

func TestParseModelsText(t *testing.T) {
	out := `You are not authenticated.

Default model: grok-4.6

Available models:
  * grok-4.6 (default)
  - grok-4.5
`
	models, defaultID := parseModelsText(out)
	require.Len(t, models, 2)
	assert.Equal(t, "grok-4.6", defaultID)
	assert.Equal(t, "grok-4.6", models[0].ID)
	assert.Equal(t, "(default)", models[0].Description)
	assert.Equal(t, "grok-4.5", models[1].ID)
}

func TestStaticModelsAndFallbackEfforts(t *testing.T) {
	opts := staticModels(fallbackModels)
	require.NotEmpty(t, opts)
	// The fallback list is newest-first, so the fallback default is the latest
	// model rather than a pinned version.
	assert.Equal(t, "grok-4.7", opts[0].Name)
	assert.Equal(t, "default", opts[0].Desc)

	// An empty catalog falls back to the conservative effort union.
	a := &Agent{activeIdx: -1, model: "grok-4.6"}
	assert.Equal(t, fallbackEfforts, a.modelEffortsLocked())

	// A populated catalog reports the union in catalog order, deduplicated.
	a.catalogCache, _ = modelsFromACPFrame([]byte(acpInitializeFrame))
	assert.Equal(t, []string{"xhigh", "high", "medium", "low"}, a.modelEffortsLocked())
	assert.Equal(t, []string{"high", "medium", "low"}, a.effortsForModel("grok-4.5"))
	assert.Equal(t, "grok-4.6", a.effectiveModelLocked())

	// With nothing pinned, GetModel reports the CLI's own default from the cache.
	a2 := &Agent{activeIdx: -1}
	assert.Equal(t, "", a2.GetModel(), "no cache → nothing to report, not a guess")
	a2.catalogCache, a2.catalogDefault = modelsFromACPFrame([]byte(acpInitializeFrame))
	assert.Equal(t, "grok-4.6", a2.GetModel())
}

func TestValidateSessionRuntime(t *testing.T) {
	a := &Agent{cmd: "grok", activeIdx: -1}
	a.catalogMu.Lock()
	a.catalogCache, _ = modelsFromACPFrame([]byte(acpInitializeFrame))
	a.catalogAt = testNow()
	a.catalogMu.Unlock()

	require.NoError(t, a.ValidateSessionRuntime("grok-4.6", ""))
	require.NoError(t, a.ValidateSessionRuntime("grok-4.5", "low"))
	require.NoError(t, a.ValidateSessionRuntime("", ""))
	assert.Error(t, a.ValidateSessionRuntime("grok-9", ""))
	// xhigh is advertised by grok-4.6 but not by grok-4.5.
	assert.Error(t, a.ValidateSessionRuntime("grok-4.5", "xhigh"))
}

func TestReasoningEffortSetter(t *testing.T) {
	a := &Agent{activeIdx: -1}
	a.SetReasoningEffort("medium")
	assert.Equal(t, "medium", a.GetReasoningEffort())
	assert.True(t, a.PreservesSessionOnReasoningEffortChange())
}

func testNow() time.Time { return time.Now() }
