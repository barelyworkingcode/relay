// Package toolsearch decides whether web chat hides its MCP tools behind a
// search step, and holds the pure logic for it: the chat.json config, the
// skill index read from a project directory, the hidden-tool catalogue and
// its ranking. It imports no provider package; the chat provider wires it in.
package toolsearch

const (
	// MaxResults is the most tool definitions one search returns.
	MaxResults = 5
	// IndexLineMaxBytes caps a skill description in the index block.
	IndexLineMaxBytes = 160
	// UnknownContextTokens stands in for a model whose context window the
	// broker did not report.
	UnknownContextTokens = 32768
)
