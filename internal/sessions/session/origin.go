package session

import sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"

// MarkOrigins copies the Origin of the session's own user messages onto the
// matching user messages of a Claude transcript, which has no such field.
// Both lists hold the same user turns in the same order, but the transcript
// may lack some of them (compaction, a turn that never ran), so matching
// walks forward: each transcript user message takes the next own user
// message with the same plain text, and a transcript message with no match
// leaves the pointer where it was so later turns still align. The input is
// not modified.
func MarkOrigins(history, own []sessionstypes.Message) []sessionstypes.Message {
	out := make([]sessionstypes.Message, len(history))
	copy(out, history)
	next := 0
	for i := range out {
		if out[i].Role != "user" {
			continue
		}
		text := sessionstypes.ExtractTextContent(out[i])
		for j := next; j < len(own); j++ {
			if own[j].Role != "user" || sessionstypes.ExtractTextContent(own[j]) != text {
				continue
			}
			out[i].Origin = own[j].Origin
			next = j + 1
			break
		}
	}
	return out
}
