package ui

import (
	"html/template"
	"path"
	"strings"
)

// Lightweight, dependency-free syntax highlighting for the file view.
//
// Recognizes a small set of common languages by file extension and tokenizes
// content into four classes: keyword, string, comment, number. Unknown
// extensions render as plain text. The tokenizer is intentionally simple and
// is not meant to be a full parser — it is good enough for code reading.

type tokenKind uint8

const (
	tkText tokenKind = iota
	tkKeyword
	tkString
	tkComment
	tkNumber
)

func (k tokenKind) class() string {
	switch k {
	case tkKeyword:
		return "hl-k"
	case tkString:
		return "hl-s"
	case tkComment:
		return "hl-c"
	case tkNumber:
		return "hl-n"
	}
	return ""
}

type language struct {
	keywords     map[string]struct{}
	lineComments []string
	blockOpen    string
	blockClose   string
	stringDelims string // each byte is a recognized string delimiter
}

// highlightedLine is the per-line render result, paired with its 1-indexed
// number for the gutter.
type highlightedLine struct {
	Num  int
	HTML template.HTML
}

func makeKeywords(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

var (
	langGo = &language{
		keywords: makeKeywords(
			"break", "case", "chan", "const", "continue", "default", "defer",
			"else", "fallthrough", "for", "func", "go", "goto", "if", "import",
			"interface", "map", "package", "range", "return", "select", "struct",
			"switch", "type", "var", "true", "false", "nil", "iota",
		),
		lineComments: []string{"//"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "\"'`",
	}
	langC = &language{
		keywords: makeKeywords(
			"auto", "break", "case", "char", "const", "continue", "default",
			"do", "double", "else", "enum", "extern", "float", "for", "goto",
			"if", "inline", "int", "long", "register", "restrict", "return",
			"short", "signed", "sizeof", "static", "struct", "switch", "typedef",
			"union", "unsigned", "void", "volatile", "while", "true", "false",
			"NULL", "class", "namespace", "template", "typename", "public",
			"private", "protected", "virtual", "new", "delete", "this", "nullptr",
		),
		lineComments: []string{"//"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "\"'",
	}
	langJS = &language{
		keywords: makeKeywords(
			"async", "await", "break", "case", "catch", "class", "const",
			"continue", "debugger", "default", "delete", "do", "else", "export",
			"extends", "finally", "for", "from", "function", "if", "import", "in",
			"instanceof", "let", "new", "null", "of", "return", "super", "switch",
			"this", "throw", "try", "typeof", "var", "void", "while", "with",
			"yield", "true", "false", "undefined",
			// TS extras
			"interface", "type", "enum", "implements", "readonly", "public",
			"private", "protected", "namespace", "as",
		),
		lineComments: []string{"//"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "\"'`",
	}
	langPython = &language{
		keywords: makeKeywords(
			"and", "as", "assert", "async", "await", "break", "class", "continue",
			"def", "del", "elif", "else", "except", "finally", "for", "from",
			"global", "if", "import", "in", "is", "lambda", "nonlocal", "not",
			"or", "pass", "raise", "return", "try", "while", "with", "yield",
			"True", "False", "None", "self",
		),
		lineComments: []string{"#"},
		stringDelims: "\"'",
	}
	langRust = &language{
		keywords: makeKeywords(
			"as", "async", "await", "break", "const", "continue", "crate", "dyn",
			"else", "enum", "extern", "false", "fn", "for", "if", "impl", "in",
			"let", "loop", "match", "mod", "move", "mut", "pub", "ref", "return",
			"self", "Self", "static", "struct", "super", "trait", "true", "type",
			"unsafe", "use", "where", "while",
		),
		lineComments: []string{"//"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "\"'",
	}
	langJava = &language{
		keywords: makeKeywords(
			"abstract", "assert", "boolean", "break", "byte", "case", "catch",
			"char", "class", "const", "continue", "default", "do", "double",
			"else", "enum", "extends", "final", "finally", "float", "for", "goto",
			"if", "implements", "import", "instanceof", "int", "interface", "long",
			"native", "new", "package", "private", "protected", "public", "return",
			"short", "static", "strictfp", "super", "switch", "synchronized",
			"this", "throw", "throws", "transient", "try", "void", "volatile",
			"while", "true", "false", "null", "var",
		),
		lineComments: []string{"//"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "\"'",
	}
	langShell = &language{
		keywords: makeKeywords(
			"if", "then", "else", "elif", "fi", "case", "esac", "for", "while",
			"until", "do", "done", "in", "function", "return", "break", "continue",
			"local", "export", "readonly", "declare", "unset", "shift", "set",
			"true", "false",
		),
		lineComments: []string{"#"},
		stringDelims: "\"'",
	}
	langRuby = &language{
		keywords: makeKeywords(
			"BEGIN", "END", "alias", "and", "begin", "break", "case", "class",
			"def", "defined?", "do", "else", "elsif", "end", "ensure", "false",
			"for", "if", "in", "module", "next", "nil", "not", "or", "redo",
			"rescue", "retry", "return", "self", "super", "then", "true", "undef",
			"unless", "until", "when", "while", "yield",
		),
		lineComments: []string{"#"},
		stringDelims: "\"'",
	}
	langYAML = &language{
		lineComments: []string{"#"},
		stringDelims: "\"'",
	}
	langJSON = &language{
		stringDelims: "\"",
	}
	langSQL = &language{
		keywords: makeKeywords(
			"SELECT", "FROM", "WHERE", "INSERT", "INTO", "VALUES", "UPDATE",
			"SET", "DELETE", "CREATE", "TABLE", "INDEX", "VIEW", "DROP", "ALTER",
			"ADD", "COLUMN", "PRIMARY", "KEY", "FOREIGN", "REFERENCES", "JOIN",
			"INNER", "LEFT", "RIGHT", "OUTER", "ON", "AS", "AND", "OR", "NOT",
			"NULL", "IS", "IN", "LIKE", "BETWEEN", "ORDER", "BY", "GROUP",
			"HAVING", "LIMIT", "OFFSET", "DISTINCT", "UNION", "ALL", "CASE",
			"WHEN", "THEN", "ELSE", "END", "BEGIN", "COMMIT", "ROLLBACK",
			"select", "from", "where", "insert", "into", "values", "update",
			"set", "delete", "create", "table", "index", "view", "drop", "alter",
			"add", "column", "primary", "key", "foreign", "references", "join",
			"inner", "left", "right", "outer", "on", "as", "and", "or", "not",
			"null", "is", "in", "like", "between", "order", "by", "group",
			"having", "limit", "offset", "distinct", "union", "all", "case",
			"when", "then", "else", "end", "begin", "commit", "rollback",
		),
		lineComments: []string{"--"},
		blockOpen:    "/*", blockClose: "*/",
		stringDelims: "'\"",
	}
)

// detectLanguage returns the language for a file path, or nil if the
// extension is not recognized.
func detectLanguage(p string) *language {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".go":
		return langGo
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp":
		return langC
	case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx":
		return langJS
	case ".py":
		return langPython
	case ".rs":
		return langRust
	case ".java":
		return langJava
	case ".sh", ".bash", ".zsh":
		return langShell
	case ".rb":
		return langRuby
	case ".yaml", ".yml":
		return langYAML
	case ".json":
		return langJSON
	case ".sql":
		return langSQL
	}
	switch path.Base(p) {
	case "Dockerfile", "Makefile":
		return langShell
	}
	return nil
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func isNumStart(b byte) bool {
	return b >= '0' && b <= '9'
}

// scanString consumes a string literal starting at s[i] (a known delimiter)
// and returns the index just past the closing delimiter (or end-of-input/
// newline for unterminated literals). Handles backslash escapes for normal
// quotes; backticks are treated as raw and may span lines.
func scanString(s string, i int) int {
	quote := s[i]
	raw := quote == '`'
	j := i + 1
	for j < len(s) {
		c := s[j]
		if !raw && c == '\\' && j+1 < len(s) {
			j += 2
			continue
		}
		if c == quote {
			return j + 1
		}
		if !raw && c == '\n' {
			return j
		}
		j++
	}
	return j
}

// scanNumber consumes a numeric literal starting at s[i]. Accepts digits,
// hex digits, and the usual base/exponent decorations.
func scanNumber(s string, i int) int {
	j := i
	for j < len(s) {
		c := s[j]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		case c == 'x', c == 'X', c == 'o', c == 'O', c == 'b', c == 'B':
		case c == '.', c == '_':
		default:
			return j
		}
		j++
	}
	return j
}

func tokenize(s string, l *language) []struct {
	kind tokenKind
	text string
} {
	type tok = struct {
		kind tokenKind
		text string
	}
	var out []tok
	emit := func(k tokenKind, t string) {
		if t == "" {
			return
		}
		// Coalesce adjacent text tokens for tighter HTML output.
		if k == tkText && len(out) > 0 && out[len(out)-1].kind == tkText {
			out[len(out)-1].text += t
			return
		}
		out = append(out, tok{k, t})
	}

	i := 0
	for i < len(s) {
		// Block comment.
		if l.blockOpen != "" && strings.HasPrefix(s[i:], l.blockOpen) {
			rest := s[i+len(l.blockOpen):]
			idx := strings.Index(rest, l.blockClose)
			var end int
			if idx < 0 {
				end = len(s)
			} else {
				end = i + len(l.blockOpen) + idx + len(l.blockClose)
			}
			emit(tkComment, s[i:end])
			i = end
			continue
		}
		// Line comment.
		matched := false
		for _, lc := range l.lineComments {
			if strings.HasPrefix(s[i:], lc) {
				nl := strings.IndexByte(s[i:], '\n')
				var end int
				if nl < 0 {
					end = len(s)
				} else {
					end = i + nl
				}
				emit(tkComment, s[i:end])
				i = end
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		c := s[i]
		// String literal.
		if strings.IndexByte(l.stringDelims, c) >= 0 {
			end := scanString(s, i)
			emit(tkString, s[i:end])
			i = end
			continue
		}
		// Number.
		if isNumStart(c) {
			// Don't start a number inside an identifier (e.g. "foo123").
			if len(out) == 0 || !endsIdent(out[len(out)-1]) {
				end := scanNumber(s, i)
				emit(tkNumber, s[i:end])
				i = end
				continue
			}
		}
		// Identifier / keyword.
		if isIdentStart(c) {
			j := i + 1
			for j < len(s) && isIdentCont(s[j]) {
				j++
			}
			word := s[i:j]
			if l.keywords != nil {
				if _, ok := l.keywords[word]; ok {
					emit(tkKeyword, word)
					i = j
					continue
				}
			}
			emit(tkText, word)
			i = j
			continue
		}
		// Anything else: a single character of plain text.
		emit(tkText, s[i:i+1])
		i++
	}
	return out
}

func endsIdent(t struct {
	kind tokenKind
	text string
}) bool {
	if t.text == "" {
		return false
	}
	return isIdentCont(t.text[len(t.text)-1])
}

// highlight tokenizes content according to the language inferred from path
// and returns one entry per line, ready to render in the file view template.
// If the language is unknown or content is empty, it falls back to plain
// HTML-escaped lines.
func highlight(content []byte, p string) []highlightedLine {
	if len(content) == 0 {
		return nil
	}
	src := strings.TrimRight(string(content), "\n")
	lang := detectLanguage(p)
	if lang == nil {
		raw := strings.Split(src, "\n")
		out := make([]highlightedLine, len(raw))
		for i, l := range raw {
			out[i] = highlightedLine{
				Num:  i + 1,
				HTML: template.HTML(template.HTMLEscapeString(l)),
			}
		}
		return out
	}

	tokens := tokenize(src, lang)

	var (
		lines []highlightedLine
		buf   strings.Builder
	)
	flushLine := func() {
		lines = append(lines, highlightedLine{
			Num:  len(lines) + 1,
			HTML: template.HTML(buf.String()),
		})
		buf.Reset()
	}
	for _, t := range tokens {
		class := t.kind.class()
		// Tokens may contain newlines (block comments, raw strings); split
		// across lines so each line gets its own balanced span.
		parts := strings.Split(t.text, "\n")
		for pi, part := range parts {
			if part != "" {
				if class == "" {
					buf.WriteString(template.HTMLEscapeString(part))
				} else {
					buf.WriteString(`<span class="`)
					buf.WriteString(class)
					buf.WriteString(`">`)
					buf.WriteString(template.HTMLEscapeString(part))
					buf.WriteString(`</span>`)
				}
			}
			if pi < len(parts)-1 {
				flushLine()
			}
		}
	}
	flushLine()
	return lines
}
