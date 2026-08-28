package mission

import "strings"

// questionScanLines is how far back from the end of a closing message the ask is
// looked for. An agent that asks something puts it last, above at most a short list
// of options.
const questionScanLines = 12

// decoration is the markdown an ask may be wrapped in, stripped before a line is
// tested and before it is stored. List markers are deliberately not in the set: they
// separate the options and are worth keeping in a card subtitle.
const decoration = "*_`#> \t"

// askOpeners start a line that hands a decision back to the human even though it
// does not end in a question mark, which is what "Should I:" above a numbered list
// looks like.
//
// The list is deliberately short. Every entry has to be a phrase that cannot
// plausibly open a sign-off: "let me know how you want to proceed" is a question,
// while "let me know if you want anything else" is what an agent says after
// finishing, and only the first form is here.
var askOpeners = []string{
	"should i",
	"shall i",
	"do you want",
	"would you like",
	"want me to",
	"which would you",
	"how would you like",
	"let me know how",
	"let me know which",
	"please confirm",
	"please pick",
	"please choose",
	"confirm whether",
	"tell me which",
}

// closingQuestion returns the question a turn ended on, or "" when it ended on a
// statement.
//
// This is the only place q reads an agent's prose, and it earns the exception.
// At the hook level a turn that ends by asking "should I do A or B?" is
// indistinguishable from one that ends with the work done: both are a Stop, with the
// agent left parked at its prompt. Without reading the message the first kind reads
// as "ready for debrief", which is the board lying in the direction [domain.Status]'s
// precedence table calls the worst one.
//
// The scan runs backwards over the tail of the message, stepping over list items and
// their wrapped continuations, because an agent offering options puts the question
// above them rather than below. A question buried mid-paragraph with prose after it is
// deliberately not chased: the false positives that would take — an agent explaining
// itself rhetorically — cost more than the case they would catch.
func closingQuestion(message string) string {
	lines := tailLines(message, questionScanLines)

	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].continues || isListItem(lines[i].text) {
			continue
		}

		if !asksSomething(lines[i].clean) {
			return ""
		}

		// The options below the ask are part of it: "Should I:" on its own says
		// nothing, while the same line with its two numbered choices is the question.
		parts := make([]string, 0, len(lines)-i)
		for _, line := range lines[i:] {
			parts = append(parts, line.clean)
		}

		return strings.Join(parts, " ")
	}

	return ""
}

// msgLine is one line of a closing message.
type msgLine struct {
	// text is the line with its surrounding whitespace removed, which is what the
	// list-marker test needs to see.
	text string
	// clean is text with its wrapping markdown removed, which is what is tested for
	// an ask and stored on the card.
	clean string
	// continues reports that the raw line was indented, which is how a list item
	// that wrapped over several lines is told apart from a new statement.
	continues bool
}

// tailLines returns up to n of the message's trailing non-blank lines.
func tailLines(message string, n int) []msgLine {
	var lines []msgLine

	for raw := range strings.SplitSeq(message, "\n") {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}

		lines = append(lines, msgLine{
			text:      text,
			clean:     strings.Trim(text, decoration),
			continues: raw != strings.TrimLeft(raw, " \t"),
		})
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return lines
}

// asksSomething reports whether one line hands a decision to the human.
func asksSomething(line string) bool {
	if strings.HasSuffix(line, "?") {
		return true
	}

	lower := strings.ToLower(line)
	for _, opener := range askOpeners {
		if strings.HasPrefix(lower, opener) {
			return true
		}
	}

	return false
}

// isListItem reports whether a line is a bullet or a numbered option.
//
// A marker has to be followed by a space, so that "**Should I:**" is read as an ask
// rather than as a list item that happens to start with an asterisk.
func isListItem(line string) bool {
	for _, bullet := range []string{"- ", "* ", "+ ", "• "} {
		if strings.HasPrefix(line, bullet) {
			return true
		}
	}

	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}

	if digits == 0 || digits+1 >= len(line) {
		return false
	}

	return (line[digits] == '.' || line[digits] == ')') && line[digits+1] == ' '
}
