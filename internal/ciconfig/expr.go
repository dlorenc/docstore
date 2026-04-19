package ciconfig

import (
	"fmt"
	"strings"
)

// EvalIf evaluates an if: expression string against the given TriggerContext.
//
// Supported syntax:
//   - String literals: "value" or 'value'
//   - Context field access: event.type, event.branch, event.base_branch, event.proposal_id
//   - Equality:   event.type == "push"
//   - Inequality: event.type != "push"
//   - Logical AND: expr && expr
//   - Logical OR:  expr || expr
//   - Parentheses for grouping
//
// Empty expr returns true (no condition = always run).
// Unknown field references return an error (treated as false with a warning by the caller).
// Malformed expressions return an error.
func EvalIf(expr string, ctx TriggerContext) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}
	p := &parser{input: expr, pos: 0, ctx: ctx}
	result, err := p.parseOr()
	if err != nil {
		return false, err
	}
	p.skipWS()
	if p.pos != len(p.input) {
		return false, fmt.Errorf("unexpected trailing input at position %d: %q", p.pos, p.input[p.pos:])
	}
	return result, nil
}

// parser holds the parsing state.
type parser struct {
	input string
	pos   int
	ctx   TriggerContext
}

func (p *parser) skipWS() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) peek(s string) bool {
	return strings.HasPrefix(p.input[p.pos:], s)
}

// consume advances past s if it is next in input. Returns true if consumed.
func (p *parser) consume(s string) bool {
	if strings.HasPrefix(p.input[p.pos:], s) {
		p.pos += len(s)
		return true
	}
	return false
}

// parseOr handles expr || expr (lowest precedence).
func (p *parser) parseOr() (bool, error) {
	left, err := p.parseAnd()
	if err != nil {
		return false, err
	}
	for {
		p.skipWS()
		if !p.consume("||") {
			break
		}
		right, err := p.parseAnd()
		if err != nil {
			return false, err
		}
		left = left || right
	}
	return left, nil
}

// parseAnd handles expr && expr.
func (p *parser) parseAnd() (bool, error) {
	left, err := p.parseComparison()
	if err != nil {
		return false, err
	}
	for {
		p.skipWS()
		if !p.consume("&&") {
			break
		}
		right, err := p.parseComparison()
		if err != nil {
			return false, err
		}
		left = left && right
	}
	return left, nil
}

// parseComparison handles value == value, value != value, or parenthesized expr.
func (p *parser) parseComparison() (bool, error) {
	p.skipWS()

	// Parenthesized sub-expression.
	if p.pos < len(p.input) && p.input[p.pos] == '(' {
		p.pos++
		result, err := p.parseOr()
		if err != nil {
			return false, err
		}
		p.skipWS()
		if p.pos >= len(p.input) || p.input[p.pos] != ')' {
			return false, fmt.Errorf("expected ')' at position %d", p.pos)
		}
		p.pos++
		return result, nil
	}

	left, err := p.parseValue()
	if err != nil {
		return false, err
	}

	p.skipWS()

	var op string
	switch {
	case p.consume("=="):
		op = "=="
	case p.consume("!="):
		op = "!="
	default:
		// No operator — treat a bare boolean field as truthy if non-empty.
		return left != "", nil
	}

	right, err := p.parseValue()
	if err != nil {
		return false, err
	}

	switch op {
	case "==":
		return left == right, nil
	case "!=":
		return left != right, nil
	default:
		return false, fmt.Errorf("unknown operator %q", op)
	}
}

// parseValue parses a string literal (single or double quoted) or a field reference.
// Returns the string value of the token.
func (p *parser) parseValue() (string, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return "", fmt.Errorf("unexpected end of expression at position %d", p.pos)
	}

	ch := p.input[p.pos]

	// Double-quoted string literal.
	if ch == '"' {
		return p.parseStringLiteral('"')
	}

	// Single-quoted string literal.
	if ch == '\'' {
		return p.parseStringLiteral('\'')
	}

	// Field reference: must start with a letter or underscore.
	if isIdentStart(ch) {
		return p.parseFieldRef()
	}

	return "", fmt.Errorf("unexpected character %q at position %d", ch, p.pos)
}

func (p *parser) parseStringLiteral(quote byte) (string, error) {
	p.pos++ // skip opening quote
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != quote {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return "", fmt.Errorf("unterminated string literal starting at position %d", start-1)
	}
	val := p.input[start:p.pos]
	p.pos++ // skip closing quote
	return val, nil
}

// parseFieldRef parses a dotted identifier like event.type and resolves it
// against the TriggerContext.
func (p *parser) parseFieldRef() (string, error) {
	start := p.pos
	for p.pos < len(p.input) && isIdentChar(p.input[p.pos]) {
		p.pos++
	}
	field := p.input[start:p.pos]

	val, err := p.resolveField(field)
	if err != nil {
		return "", err
	}
	return val, nil
}

// resolveField maps a dotted field name to a value from TriggerContext.
func (p *parser) resolveField(field string) (string, error) {
	switch field {
	case "event.type":
		return p.ctx.Type, nil
	case "event.branch":
		return p.ctx.Branch, nil
	case "event.base_branch":
		return p.ctx.BaseBranch, nil
	case "event.proposal_id":
		return p.ctx.ProposalID, nil
	default:
		return "", fmt.Errorf("unknown field reference %q", field)
	}
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '.'
}
