package agent

// Conversation manages the message history for an agent run.
// Unlike the old implementation that stored []anthropic.Message,
// this uses the RunItem-based history format.
type Conversation struct {
	Items []RunItem
}

// NewConversation creates an empty conversation.
func NewConversation() *Conversation {
	return &Conversation{}
}

// NewConversationFromItems creates a conversation with existing items.
func NewConversationFromItems(items []RunItem) *Conversation {
	return &Conversation{Items: items}
}

// Append adds items to the conversation.
func (c *Conversation) Append(items ...RunItem) {
	c.Items = append(c.Items, items...)
}

// LastText returns the text of the last message item.
func (c *Conversation) LastText() string {
	return Items.ExtractLastText(c.Items)
}

// AllText returns all concatenated message text.
func (c *Conversation) AllText() string {
	return Items.ExtractText(c.Items)
}

// ToolCalls returns all tool call data.
func (c *Conversation) ToolCalls() []ToolCallData {
	return Items.ExtractToolCalls(c.Items)
}

// Len returns the number of items.
func (c *Conversation) Len() int {
	return len(c.Items)
}

// Clear removes all items.
func (c *Conversation) Clear() {
	c.Items = nil
}

// Copy returns a deep copy of the conversation.
func (c *Conversation) Copy() *Conversation {
	items := make([]RunItem, len(c.Items))
	copy(items, c.Items)
	return &Conversation{Items: items}
}
