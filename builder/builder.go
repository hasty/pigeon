// Package builder generates the parser code for a given grammar. It makes
// no attempt to verify the correctness of the grammar.
package builder

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/fy0/pigeon/ast"
)

const codeGeneratedComment = "// Code generated by pigeon; DO NOT EDIT.\n\n"

func (b *builder) templateRenderBase(text string, trim bool, m map[string]any) string {
	t, err := template.New("").Parse(text)

	var out bytes.Buffer
	err = t.Execute(&out, m)

	if err != nil {
		panic(err)
	}

	if trim {
		return strings.TrimSpace(out.String())
	} else {
		return out.String()
	}
}

func (b *builder) templateRender(text string, trim bool) string {
	return b.templateRenderBase(text, trim, map[string]any{
		"target":      b.target,
		"grammarOnly": b.grammarOnly,
	})
}

// generated function templates
var (
	callCodeFuncTemplate = `func (p *parser) call{{.funcName}}() any {
{{ if .useStack }} stack := p.vstack[len(p.vstack)-1]; {{ end }} return (func (c *current, {{.paramsDef}}) any {
		{{.code}}
		return nil
	})(&p.cur, {{.paramsCall}})
}
`
	callPredFuncTemplate = `func (p *parser) call{{.funcName}}() bool {
{{ if .useStack }} stack := p.vstack[len(p.vstack)-1]; {{ end }}	return (func (c *current, {{.paramsDef}}) bool {
		{{.code}}
	})(&p.cur, {{.paramsCall}})
}
`
)

// Option is a function that can set an option on the builder. It returns
// the previous setting as an Option.
type Option func(*builder) Option

func OptimizeRefExprByIndex(enable bool) Option {
	return func(b *builder) Option {
		prev := b.iRefEnable
		b.iRefEnable = enable
		return OptimizeRefExprByIndex(prev)
	}
}

func GrammarOnly(enable bool) Option {
	return func(b *builder) Option {
		prev := b.grammarOnly
		b.grammarOnly = enable
		return GrammarOnly(prev)
	}
}

func RunFuncPrefix(value string) Option {
	return func(b *builder) Option {
		prev := b.funcPrefix
		b.funcPrefix = value
		return RunFuncPrefix(prev)
	}
}

func GrammarName(value string) Option {
	return func(b *builder) Option {
		prev := b.grammarName
		b.grammarName = value
		return GrammarName(prev)
	}
}

// ReceiverName returns an option that specifies the receiver exprType to
// use for the current struct (which is the struct on which all code blocks
// except the initializer are generated).
func ReceiverName(nm string) Option {
	return func(b *builder) Option {
		prev := b.recvName
		b.recvName = nm
		return ReceiverName(prev)
	}
}

// Optimize returns an option that specifies the optimize option
// If optimize is true, the Debug and Memoize code is completely
// removed from the resulting parser
func Optimize(optimize bool) Option {
	return func(b *builder) Option {
		prev := b.optimize
		b.optimize = optimize
		return Optimize(prev)
	}
}

// Nolint returns an option that specifies the nolint option
// If nolint is true, special '// nolint: ...' comments are added
// to the generated parser to suppress warnings by gometalinter or golangci-lint.
func Nolint(nolint bool) Option {
	return func(b *builder) Option {
		prev := b.nolint
		b.nolint = nolint
		return Optimize(prev)
	}
}

// BuildParser builds the PEG parser using the provider grammar. The code is
// written to the specified w.
func BuildParser(w io.Writer, g *ast.Grammar, opts ...Option) error {
	b := &builder{w: w, recvName: "c", target: "go"}
	b.setOptions(opts)
	b.globalState = false
	return b.buildParser(g)
}

type builder struct {
	w   io.Writer
	err error

	// options
	recvName          string
	optimize          bool
	globalState       bool
	nolint            bool
	haveLeftRecursion bool

	ruleName  string
	exprIndex int
	argsStack [][]string

	target     string
	rangeTable bool
	grammarMap bool
	entrypoint string

	iRefEnable     bool
	iRefCodeEnable bool

	funcPrefix  string
	grammarName string
	grammarOnly bool

	ruleName2Index map[string]*ExprInfo
}

func (b *builder) setOptions(opts []Option) {
	for _, opt := range opts {
		opt(b)
	}
}

func (b *builder) buildParser(grammar *ast.Grammar) error {
	haveLeftRecursion, err := PrepareGrammar(grammar)
	if err != nil {
		return fmt.Errorf("incorrect grammar: %w", err)
	}
	if haveLeftRecursion {
		return fmt.Errorf("incorrect grammar: %w", ErrHaveLeftRecursion)
	}
	b.haveLeftRecursion = haveLeftRecursion

	b.writeInit(grammar.Init)
	if !b.grammarMap {
		b.writeGrammar(grammar)
	} else {
		b.writeGrammar2(grammar)
	}
	for _, rule := range grammar.Rules {
		b.writeRuleCode(rule)
	}

	if !b.grammarOnly {
		b.writeStaticCode()
	}

	return b.err
}

func (b *builder) writeInit(init *ast.CodeBlock) {
	if init == nil {
		return
	}

	// remove opening and closing braces
	val := codeGeneratedComment + b.templateRender(init.Val[1:len(init.Val)-1], false)
	b.writelnf("%s", val)
}

func (b *builder) writeGrammar(g *ast.Grammar) {
	// transform the ast grammar to the self-contained, no dependency version
	// of the parser-generator grammar.

	m := map[string]*ExprInfo{}

	for index, r := range g.Rules {
		info := b.getExprInfo(r.Expr)
		info.index = index
		m[r.Name.Val] = info
	}
	b.ruleName2Index = m

	b.writelnf("var %s = &grammar {", b.grammarName)
	b.writelnf("\trules: []*rule{")
	for _, r := range g.Rules {
		b.writeRule(r)
	}
	b.writelnf("\t},")
	b.writelnf("}")
}

func (b *builder) writeGrammar2(g *ast.Grammar) {
	// transform the ast grammar to the self-contained, no dependency version
	// of the parser-generator grammar.
	b.writelnf("var g = map[string]*rule {")
	for _, r := range g.Rules {
		b.writeRule(r)
	}
	b.writelnf("}")
}

func (b *builder) writeRulePos(pos ast.Pos) {
	// SetRulePos
	// b.writelnf("\tpos: position{line: %d, col: %d, offset: %d},", pos.Line, pos.Col, pos.Off)
}

func (b *builder) writeRule(r *ast.Rule) {
	if r == nil || r.Name == nil {
		return
	}

	b.exprIndex = 0
	b.ruleName = r.Name.Val

	if b.entrypoint == "" {
		b.entrypoint = r.Name.Val
	}

	if b.grammarMap {
		b.writelnf("%q: {", r.Name.Val)
	} else {
		b.writelnf("{")
	}
	b.writelnf("\tname: %q,", r.Name.Val)
	if r.DisplayName != nil && r.DisplayName.Val != "" {
		b.writelnf("\tdisplayName: %q,", r.DisplayName.Val)
	}
	b.writeRulePos(r.Pos())
	b.writef("\texpr: ")
	b.writeExpr(r.Expr)
	if b.haveLeftRecursion {
		b.writelnf("\tleader: %t,", r.Leader)
		b.writelnf("\tleftRecursive: %t,", r.LeftRecursive)
	}
	b.writelnf("},")
}

type ExprInfo struct {
	index    int
	name     string
	exprType string
}

func (b *builder) getExprInfo(expr ast.Expression) *ExprInfo {
	switch expr.(type) {
	case *ast.ActionExpr:
		return &ExprInfo{exprType: "actionExpr"}
	case *ast.AndCodeExpr:
		return &ExprInfo{exprType: "andCodeExpr"}
	case *ast.AndExpr:
		return &ExprInfo{exprType: "andExpr"}
	case *ast.AnyMatcher:
		return &ExprInfo{exprType: "anyMatcher"}
	case *ast.CharClassMatcher:
		return &ExprInfo{exprType: "charClassMatcher"}
	case *ast.ChoiceExpr:
		return &ExprInfo{exprType: "choiceExpr"}
	case *ast.LabeledExpr:
		return &ExprInfo{exprType: "labeledExpr"}
	case *ast.LitMatcher:
		return &ExprInfo{exprType: "litMatcher"}
	case *ast.NotCodeExpr:
		return &ExprInfo{exprType: "notCodeExpr"}
	case *ast.NotExpr:
		return &ExprInfo{exprType: "notExpr"}
	case *ast.OneOrMoreExpr:
		return &ExprInfo{exprType: "oneOrMoreExpr"}
	case *ast.RecoveryExpr:
		return &ExprInfo{exprType: "recoveryExpr"}
	case *ast.RuleRefExpr:
		return &ExprInfo{exprType: "ruleRefExpr"}
	case *ast.SeqExpr:
		return &ExprInfo{exprType: "seqExpr"}
	case *ast.CodeExpr:
		return &ExprInfo{exprType: "codeExpr"}
	case *ast.ThrowExpr:
		return &ExprInfo{exprType: "throwExpr"}
	case *ast.ZeroOrMoreExpr:
		return &ExprInfo{exprType: "zeroOrMoreExpr"}
	case *ast.ZeroOrOneExpr:
		return &ExprInfo{exprType: "zeroOrOneExpr"}
	default:
		return nil
	}
}

func (b *builder) writeExpr(expr ast.Expression) {
	b.exprIndex++
	switch expr := expr.(type) {
	case *ast.ActionExpr:
		b.writeActionExpr(expr)
	case *ast.AndCodeExpr:
		b.writeAndCodeExpr(expr)
	case *ast.AndExpr:
		b.writeAndExpr(expr)
	case *ast.AnyMatcher:
		b.writeAnyMatcher(expr)
	case *ast.CharClassMatcher:
		b.writeCharClassMatcher(expr)
	case *ast.ChoiceExpr:
		b.writeChoiceExpr(expr)
	case *ast.LabeledExpr:
		b.writeLabeledExpr(expr)
	case *ast.LitMatcher:
		b.writeLitMatcher(expr)
	case *ast.NotCodeExpr:
		b.writeNotCodeExpr(expr)
	case *ast.NotExpr:
		b.writeNotExpr(expr)
	case *ast.OneOrMoreExpr:
		b.writeOneOrMoreExpr(expr)
	case *ast.RecoveryExpr:
		b.writeRecoveryExpr(expr)
	case *ast.RuleRefExpr:
		b.writeRuleRefExpr(expr)
	case *ast.SeqExpr:
		b.writeSeqExpr(expr)
	case *ast.CodeExpr:
		b.writeCodeExpr(expr)
	case *ast.ThrowExpr:
		b.writeThrowExpr(expr)
	case *ast.ZeroOrMoreExpr:
		b.writeZeroOrMoreExpr(expr)
	case *ast.ZeroOrOneExpr:
		b.writeZeroOrOneExpr(expr)
	default:
		b.err = fmt.Errorf("builder: unknown expression type %T", expr)
	}
}

func (b *builder) writeAndExpr(and *ast.AndExpr) {
	if and == nil {
		b.writelnf("nil,")
		return
	}
	if and.Logical {
		b.writelnf("&andLogicalExpr{")
	} else {
		b.writelnf("&andExpr{")
	}
	b.writeRulePos(and.Pos())
	b.writef("\texpr: ")
	b.writeExpr(and.Expr)
	b.writelnf("},")
}

func (b *builder) writeAnyMatcher(any *ast.AnyMatcher) {
	if any == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&anyMatcher{")
	pos := any.Pos()
	b.writelnf("\tline: %d, col: %d, offset: %d,", pos.Line, pos.Col, pos.Off)
	b.writelnf("},")
}
func (b *builder) writeActionExpr(act *ast.ActionExpr) {
	if act == nil {
		b.writelnf("nil,")
		return
	}
	if act.FuncIx == 0 {
		act.FuncIx = b.exprIndex
	}
	b.writelnf("&actionExpr{")
	b.writeRulePos(act.Pos())
	b.writelnf("\trun: (*parser).call%s,", b.funcName(act.FuncIx))
	b.writef("\texpr: ")
	b.writeExpr(act.Expr)
	b.writelnf("},")
}

func (b *builder) writeAndCodeExpr(and *ast.AndCodeExpr) {
	if and == nil {
		b.writelnf("nil,")
		return
	}
	b.writef("&andCodeExpr{")
	pos := and.Pos()
	if and.FuncIx == 0 {
		and.FuncIx = b.exprIndex
	}
	b.writeRulePos(pos)
	b.writef("\trun: (*parser).call%s,", b.funcName(and.FuncIx))
	b.writelnf("},")
}

func (b *builder) writeCharClassMatcher(ch *ast.CharClassMatcher) {
	if ch == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&charClassMatcher{")
	pos := ch.Pos()
	b.writeRulePos(pos)
	b.writelnf("\tval: %q,", ch.Val)
	if len(ch.Chars) > 0 {
		b.writef("\tchars: []rune{")
		for _, rn := range ch.Chars {
			if ch.IgnoreCase {
				b.writef("%q,", unicode.ToLower(rn))
			} else {
				b.writef("%q,", rn)
			}
		}
		b.writelnf("},")
	}
	if len(ch.Ranges) > 0 {
		b.writef("\tranges: []rune{")
		for _, rn := range ch.Ranges {
			if ch.IgnoreCase {
				b.writef("%q,", unicode.ToLower(rn))
			} else {
				b.writef("%q,", rn)
			}
		}
		b.writelnf("},")
	}
	if len(ch.UnicodeClasses) > 0 {
		b.rangeTable = true
		b.writef("\tclasses: []*unicode.RangeTable{")
		for _, cl := range ch.UnicodeClasses {
			// b.writef("rangeTable(%q),", cl)
			b.writef("unicode.%s,", cl)
		}
		b.writelnf("},")
	}
	b.writelnf("\tignoreCase: %t,", ch.IgnoreCase)
	b.writelnf("\tinverted: %t,", ch.Inverted)
	b.writelnf("},")
}

func (b *builder) writeCodeExpr(state *ast.CodeExpr) {
	if state == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&codeExpr{")
	pos := state.Pos()
	if state.FuncIx == 0 {
		state.FuncIx = b.exprIndex
	}
	b.writeRulePos(pos)
	b.writelnf("\trun: (*parser).call%s,", b.funcName(state.FuncIx))
	if state.NotSkip {
		b.writelnf("\tnotSkip: %v,", state.NotSkip)
	}
	b.writelnf("},")
}

// BasicLatinLookup calculates the decision results for the first 256 characters of the UTF-8 character
// set for a given set of chars, ranges and unicodeClasses to speedup the CharClassMatcher.
func BasicLatinLookup(chars, ranges []rune, unicodeClasses []string, ignoreCase bool) (basicLatinChars [128]bool) {
	for _, rn := range chars {
		if rn < 128 {
			basicLatinChars[rn] = true
			if ignoreCase {
				if unicode.IsLower(rn) {
					basicLatinChars[unicode.ToUpper(rn)] = true
				} else {
					basicLatinChars[unicode.ToLower(rn)] = true
				}
			}
		}
	}
	for i := 0; i < len(ranges); i += 2 {
		if ranges[i] < 128 {
			for j := ranges[i]; j < 128 && j <= ranges[i+1]; j++ {
				basicLatinChars[j] = true
				if ignoreCase {
					if unicode.IsLower(j) {
						basicLatinChars[unicode.ToUpper(j)] = true
					} else {
						basicLatinChars[unicode.ToLower(j)] = true
					}
				}
			}
		}
	}
	for _, cl := range unicodeClasses {
		rt := rangeTable(cl)
		for r := rune(0); r < 128; r++ {
			if unicode.Is(rt, r) {
				basicLatinChars[r] = true
			}
		}
	}
	return
}

func (b *builder) writeChoiceExpr(ch *ast.ChoiceExpr) {
	if ch == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&choiceExpr{")
	pos := ch.Pos()
	b.writeRulePos(pos)
	if len(ch.Alternatives) > 0 {
		b.writelnf("\talternatives: []any{")
		for _, alt := range ch.Alternatives {
			b.writeExpr(alt)
		}
		b.writelnf("\t},")
	}
	b.writelnf("},")
}

func (b *builder) writeLabeledExpr(lab *ast.LabeledExpr) {
	if lab == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&labeledExpr{")
	pos := lab.Pos()
	b.writeRulePos(pos)
	if lab.Label != nil && lab.Label.Val != "" {
		b.writelnf("\tlabel: %q,", lab.Label.Val)
	}
	b.writef("\texpr: ")
	b.writeExpr(lab.Expr)
	if lab.TextCapture {
		b.writelnf("\ttextCapture: %v,", lab.TextCapture)
	}
	b.writelnf("},")
}

func (b *builder) writeLitMatcher(lit *ast.LitMatcher) {
	if lit == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&litMatcher{")
	pos := lit.Pos()
	b.writeRulePos(pos)
	if lit.IgnoreCase {
		b.writelnf("\tval: %q,", strings.ToLower(lit.Val))
	} else {
		b.writelnf("\tval: %q,", lit.Val)
	}
	b.writelnf("\tignoreCase: %t,", lit.IgnoreCase)
	ignoreCaseFlag := ""
	if lit.IgnoreCase {
		ignoreCaseFlag = "i"
	}
	b.writelnf("\twant: %q,", strconv.Quote(lit.Val)+ignoreCaseFlag)
	b.writelnf("},")
}

func (b *builder) writeNotCodeExpr(not *ast.NotCodeExpr) {
	if not == nil {
		b.writelnf("nil,")
		return
	}
	b.writef("&notCodeExpr{")
	pos := not.Pos()
	if not.FuncIx == 0 {
		not.FuncIx = b.exprIndex
	}
	b.writeRulePos(pos)
	b.writef("\trun: (*parser).call%s,", b.funcName(not.FuncIx))
	b.writelnf("},")
}

func (b *builder) writeNotExpr(not *ast.NotExpr) {
	if not == nil {
		b.writelnf("nil,")
		return
	}
	if not.Logical {
		b.writelnf("&notLogicalExpr{")
	} else {
		b.writelnf("&notExpr{")
	}
	pos := not.Pos()
	b.writeRulePos(pos)
	b.writef("\texpr: ")
	b.writeExpr(not.Expr)
	b.writelnf("},")
}

func (b *builder) writeOneOrMoreExpr(one *ast.OneOrMoreExpr) {
	if one == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&oneOrMoreExpr{")
	pos := one.Pos()
	b.writeRulePos(pos)
	b.writef("\texpr: ")
	b.writeExpr(one.Expr)
	b.writelnf("},")
}

func (b *builder) writeRecoveryExpr(recover *ast.RecoveryExpr) {
	if recover == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&recoveryExpr{")
	pos := recover.Pos()
	b.writeRulePos(pos)

	b.writef("\texpr: ")
	b.writeExpr(recover.Expr)
	b.writef("\trecoverExpr: ")
	b.writeExpr(recover.RecoverExpr)
	b.writelnf("\tfailureLabel: []string{")
	for _, label := range recover.Labels {
		b.writelnf("%q,", label)
	}
	b.writelnf("\t},")
	b.writelnf("},")
}

func (b *builder) writeRuleRefExpr(ref *ast.RuleRefExpr) {
	if ref == nil {
		b.writelnf("nil,")
		return
	}
	if b.iRefEnable {
		if b.iRefCodeEnable {
			b.writef("&ruleIRefExprX{")
		} else {
			b.writef("&ruleIRefExpr{")
		}
		pos := ref.Pos()
		b.writeRulePos(pos)
		if ref.Name != nil && ref.Name.Val != "" {
			info := b.ruleName2Index[ref.Name.Val]
			b.writef("\tindex: %d /* %s */", info.index, ref.Name.Val)

			if b.iRefCodeEnable {
				exprType := info.exprType
				if exprType == "ruleRefExpr" {
					exprType = "ruleIRefExprX"
				}
				parseFnName := "parse" + strings.ToUpper(exprType[:1]) + exprType[1:]
				b.writef(", call: func(p*parser, expr any) (any, bool) { return p.%s(expr.(*rule).expr.(*%s)) }", parseFnName, exprType)
			}
		}
		b.writelnf("},")
	} else {
		b.writef("&ruleRefExpr{")
		pos := ref.Pos()
		b.writeRulePos(pos)
		if ref.Name != nil && ref.Name.Val != "" {
			b.writef("\tname: %q,", ref.Name.Val)
		}
		b.writelnf("},")
	}
}

func (b *builder) writeSeqExpr(seq *ast.SeqExpr) {
	if seq == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&seqExpr{")
	pos := seq.Pos()
	b.writeRulePos(pos)
	if len(seq.Exprs) > 0 {
		b.writelnf("\texprs: []any{")
		for _, e := range seq.Exprs {
			b.writeExpr(e)
		}
		b.writelnf("\t},")
	}
	b.writelnf("},")
}

func (b *builder) writeThrowExpr(throw *ast.ThrowExpr) {
	if throw == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&throwExpr{")
	pos := throw.Pos()
	b.writeRulePos(pos)
	b.writelnf("\tlabel: %q,", throw.Label)
	b.writelnf("},")
}

func (b *builder) writeZeroOrMoreExpr(zero *ast.ZeroOrMoreExpr) {
	if zero == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&zeroOrMoreExpr{")
	pos := zero.Pos()
	b.writeRulePos(pos)
	b.writef("\texpr: ")
	b.writeExpr(zero.Expr)
	b.writelnf("},")
}

func (b *builder) writeZeroOrOneExpr(zero *ast.ZeroOrOneExpr) {
	if zero == nil {
		b.writelnf("nil,")
		return
	}
	b.writelnf("&zeroOrOneExpr{")
	pos := zero.Pos()
	b.writeRulePos(pos)
	b.writef("\texpr: ")
	b.writeExpr(zero.Expr)
	b.writelnf("},")
}

func (b *builder) writeRuleCode(rule *ast.Rule) {
	if rule == nil || rule.Name == nil {
		return
	}

	// keep trace of the current rule, as the code blocks are created
	// in functions named "on<RuleName><#ExprIndex>".
	b.ruleName = rule.Name.Val
	b.pushArgsSet()
	b.writeExprCode(rule.Expr)
	b.popArgsSet()
}

func (b *builder) pushArgsSet() {
	b.argsStack = append(b.argsStack, nil)
}

func (b *builder) popArgsSet() {
	b.argsStack = b.argsStack[:len(b.argsStack)-1]
}

func (b *builder) addArg(arg *ast.Identifier) {
	if arg == nil {
		return
	}
	ix := len(b.argsStack) - 1
	b.argsStack[ix] = append(b.argsStack[ix], arg.Val)
}

func (b *builder) writeExprCode(expr ast.Expression) {
	switch expr := expr.(type) {
	case *ast.ActionExpr:
		b.writeExprCode(expr.Expr)
		b.writeActionExprCode(expr)

	case *ast.AndCodeExpr:
		b.writeAndCodeExprCode(expr)

	case *ast.LabeledExpr:
		b.addArg(expr.Label)
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()

	case *ast.NotCodeExpr:
		b.writeNotCodeExprCode(expr)

	case *ast.AndExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()

	case *ast.ChoiceExpr:
		for _, alt := range expr.Alternatives {
			b.pushArgsSet()
			b.writeExprCode(alt)
			b.popArgsSet()
		}

	case *ast.NotExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()

	case *ast.OneOrMoreExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()

	case *ast.RecoveryExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.writeExprCode(expr.RecoverExpr)
		b.popArgsSet()

	case *ast.SeqExpr:
		for _, sub := range expr.Exprs {
			b.writeExprCode(sub)
		}

	case *ast.CodeExpr:
		b.writeCodeExprCode(expr)

	case *ast.ZeroOrMoreExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()

	case *ast.ZeroOrOneExpr:
		b.pushArgsSet()
		b.writeExprCode(expr.Expr)
		b.popArgsSet()
	}
}

func (b *builder) writeActionExprCode(act *ast.ActionExpr) {
	if act == nil {
		return
	}
	if act.FuncIx > 0 {
		b.writeFunc(act.FuncIx, act.Code, callCodeFuncTemplate)
		act.FuncIx = 0 // already rendered, prevent duplicates
	}
}

func (b *builder) writeAndCodeExprCode(and *ast.AndCodeExpr) {
	if and == nil {
		return
	}
	if and.FuncIx > 0 {
		b.writeFunc(and.FuncIx, and.Code, callPredFuncTemplate)
		and.FuncIx = 0 // already rendered, prevent duplicates
	}
}

func (b *builder) writeNotCodeExprCode(not *ast.NotCodeExpr) {
	if not == nil {
		return
	}
	if not.FuncIx > 0 {
		b.writeFunc(not.FuncIx, not.Code, callPredFuncTemplate)
		not.FuncIx = 0 // already rendered, prevent duplicates
	}
}

func (b *builder) writeCodeExprCode(code *ast.CodeExpr) {
	if code == nil {
		return
	}
	if code.FuncIx > 0 {
		b.writeFunc(code.FuncIx, code.Code, callCodeFuncTemplate)
		code.FuncIx = 0 // already rendered, prevent duplicates
	}
}

func stringArrayUniq(items []string) []string {
	var newArray []string
	m := map[string]bool{}
	for _, i := range items {
		if !m[i] {
			m[i] = true
			newArray = append(newArray, i)
		}
	}
	return newArray
}

func (b *builder) writeFunc(funcIx int, code *ast.CodeBlock, funcTpl string) {
	if code == nil {
		return
	}
	val := b.templateRender(strings.TrimSpace(code.Val)[1:len(code.Val)-1], true)
	if len(val) > 0 && val[0] == '\n' {
		val = val[1:]
	}
	if len(val) > 0 && val[len(val)-1] == '\n' {
		val = val[:len(val)-1]
	}
	var args bytes.Buffer
	ix := len(b.argsStack) - 1
	argsInfo := stringArrayUniq(b.argsStack[ix])
	if ix >= 0 {
		for i, arg := range argsInfo {
			if i > 0 {
				args.WriteString(", ")
			}
			args.WriteString(arg)
		}
	}
	if args.Len() > 0 {
		args.WriteString(" any")
	}

	params := args.String()
	args.Reset()
	if ix >= 0 {
		for i, arg := range argsInfo {
			if i > 0 {
				args.WriteString(", ")
			}
			args.WriteString(fmt.Sprintf(`stack[%q]`, arg))
		}
	}

	b.writelnf(b.templateRenderBase(funcTpl, false, map[string]any{
		"funcName":   b.funcName(funcIx),
		"paramsDef":  params,
		"code":       val,
		"paramsCall": args.String(),
		"useStack":   len(argsInfo) > 0,
	}))
}

func (b *builder) writeStaticCode() {
	buffer := bytes.NewBufferString("")
	params := struct {
		Optimize       bool
		Nolint         bool
		SetRulePos     bool
		Entrypoint     string
		GrammarMap     bool
		IRefEnable     bool
		IRefCodeEnable bool
		NeedExprWrap   bool
		ParseExprName  string
		GrammarVarName string
	}{
		Optimize:       b.optimize,
		Nolint:         b.nolint,
		SetRulePos:     false,
		Entrypoint:     b.entrypoint,
		GrammarMap:     b.grammarMap,
		IRefEnable:     b.iRefEnable,
		IRefCodeEnable: b.iRefCodeEnable,
		NeedExprWrap:   !b.optimize || b.haveLeftRecursion,
		ParseExprName:  "parseExpr",
		GrammarVarName: b.grammarName,
	}
	if !params.NeedExprWrap {
		params.ParseExprName = "parseExprWrap"
	}
	t := template.Must(template.New("static_code").Parse(staticCode))

	err := t.Execute(buffer, params)
	if err != nil {
		// This is very unlikely to ever happen
		panic("executing template: " + err.Error())
	}

	// Clean the ==template== comments from the generated parser
	lines := strings.Split(buffer.String(), "\n")
	buffer.Reset()
	re := regexp.MustCompile(`^\s*//\s*(==template==\s*)+$`)
	reLineEnd := regexp.MustCompile(`//\s*==template==\s*$`)
	for _, line := range lines {
		if !re.MatchString(line) {
			line = reLineEnd.ReplaceAllString(line, "")
			_, err := buffer.WriteString(line + "\n")
			if err != nil {
				// This is very unlikely to ever happen
				panic("unable to write to byte buffer: " + err.Error())
			}
		}
	}

	b.writeln(buffer.String())
	// if b.rangeTable {
	// 	b.writeln(rangeTable0)
	// }
}

func (b *builder) funcName(ix int) string {
	return "_on" + b.funcPrefix + b.ruleName + "_" + strconv.Itoa(ix)
}

func (b *builder) writef(f string, args ...any) {
	if b.err == nil {
		_, b.err = fmt.Fprintf(b.w, f, args...)
	}
}

func (b *builder) writelnf(f string, args ...any) {
	b.writef(f+"\n", args...)
}

func (b *builder) writeln(f string) {
	if b.err == nil {
		_, b.err = fmt.Fprint(b.w, f+"\n")
	}
}
