package agentsdk

// GuardrailRule is the SDK-native representation of a tool guardrail rule.
// Host adapters can map CRDs, config files, or database rows into this shape.
type GuardrailRule struct {
	Name        string
	Type        string
	Regex       string
	ToolPattern string
	Action      string
	Message     string
}
