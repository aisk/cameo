// Command sync regenerates the llmconv sources from a magpie checkout.
//
//	go run ./llmconv/internal/sync /path/to/magpie
//
// Each generated file holds declarations of the upstream file with the same
// name. A file listed with keep takes only those declarations, one listed
// with drop takes everything else, and one with neither is taken whole.
// Naming a type also names its methods.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type source struct {
	file string
	keep []string
	drop []string
}

type target struct {
	from, to, pkg string
	sources       []source
	// data are files the tests read, copied as they are: paths under from,
	// put at the same paths under to
	data []string
}

const (
	upstreamModule = "github.com/yetone/magpie"
	localModule    = "github.com/aisk/cameo"
)

var targets = []target{
	{from: "internal/gateway", to: "llmconv", pkg: "llmconv", sources: []source{
		{file: "ir.go"},
		{file: "format.go"},
		{file: "chat.go", drop: []string{"buildHost"}},
		{file: "anthropic.go"},
		{file: "responses.go"},
		{file: "gemini.go"},
		{file: "gemini_signature.go", keep: []string{"sigMark", "signedID", "unsignedID", "unsignCalls", "googleSignature", "googleExtra"}},
		{file: "additional_tools.go"},
		{file: "sse.go"},
		// the Gemini wire format lives with Code Assist's envelope upstream;
		// accounts, projects and sign-in stay behind
		{file: "codeassist.go", keep: []string{
			"skipSignature", "unsafeToolID", "antigravityBaseOf", "codeAssistID", "buildCodeAssist", "buildCodeAssistSent", "thinkingConfig", "plainSchema",
			"geminiChunk", "codeAssistDecoder", "maxHeld", "callMarker", "textCall", "textArgs", "nextKey", "closeBrace",
			"isIdent", "ctrl46", "toolOfCall", "geminiNamespace", "stopFromGemini",
		}},
		{file: "gateway.go", keep: []string{
			"parse", "requiredAllowlist", "build", "decoder", "streamEncoder", "encoder", "makeEncoder", "render",
			"badRequest", "withFields", "bodyEffort",
		}},
		{file: "response_id.go", keep: []string{"usageEncoder"}},
		{file: "decide.go", keep: []string{"requestEffort"}},
		{file: "fallback.go", keep: []string{"refusedCode", "policyRefusal", "errorCode", "filterReasons", "flaggedWords", "policyCode", "refusedNote"}},
		{file: "chattidy.go", keep: []string{"partsText"}},
		{file: "continuation.go", keep: []string{"chatPrefill"}},
		{file: "toolsearch.go", keep: []string{"marshalPlain"}},
		{file: "codex_backend.go", keep: []string{"openaiItemPrefix"}},

		// Tests of the above that need no server, provider or settings. A
		// file taken whole had only such tests when it was added; one that
		// grows a test this package cannot build fails the sync's build and
		// gets a keep list.
		{file: "adaptive_test.go"},
		{file: "additional_tools_test.go", keep: []string{"TestAdditionalToolsMerged", "TestAdditionalToolsTranslated", "liteRequest"}},
		{file: "agent_message_test.go"},
		{file: "agy_agent_test.go", keep: []string{"TestAgyToolResultsAreTheUsers"}},
		{file: "aistudio_thinking_test.go", keep: []string{"TestAIStudioThinking", "TestChatThoughtTags", "TestChatThoughtTagsToAnthropic", "TestGeminiCompat"}},
		{file: "anthropic_ids_test.go"},
		{file: "anthropic_image_source_test.go", keep: []string{"TestAnthropicImageSourceFields"}},
		{file: "anthropic_schema_test.go"},
		{file: "attachments_test.go", keep: []string{"TestChatFilePart", "TestGeminiSnakeCaseParts"}},
		{file: "call_item_ids_test.go", keep: []string{"TestCustomToolCallHasCtcID"}},
		{file: "chat_tool_pairing_test.go"},
		{file: "codeassist_search_test.go", keep: []string{"TestCodeAssistDecodesGrounding", "TestCodeAssistImageModelAsksForImages"}},
		{file: "codeassist_test.go", keep: []string{
			"TestBuildCodeAssistForAntigravity", "TestBuildCodeAssistForGemini", "TestCodeAssistDecoder",
			"TestCodeAssistTextCalls", "TestCodeAssistToolImagesAfterEveryResponse", "codeAssistRequest",
		}},
		{file: "codex_custom_tools_test.go", keep: []string{"TestCodexCustomToolNamespaced", "renderCall"}},
		{file: "codex_ultra_test.go", keep: []string{"TestUltraIsSentAsMax"}},
		{file: "continuation_test.go", keep: []string{"TestBuildChatResumePrefillMarks", "TestChatPrefill"}},
		{file: "effort_test.go", keep: []string{"TestFitEffort", "TestRequestEffort"}},
		{file: "fast_test.go"},
		{file: "format_test.go", keep: []string{
			"TestResponsesTextKeepsVerbosityBesideFormat", "TestStructuredOutputOnGemini", "calendarSchema", "jsonOf",
			"sameFormat", "sdkChatParse",
		}},
		{file: "gateway_test.go", keep: []string{"TestUsagePromptCountsCacheWrites", "events"}},
		{file: "gemini_first_turn_test.go"},
		{file: "gemini_function_parts_test.go", keep: []string{"TestGeminiFunctionResponseFile", "TestGeminiFunctionResponseImage", "agyImage", "agyViewFile"}},
		{file: "gemini_image_test.go", keep: []string{"TestCodeAssistStreamsImages"}},
		{file: "gemini_signature_test.go", keep: []string{"TestSignedID"}},
		{file: "gemini_tools_test.go"},
		{file: "ids_unique_test.go"},
		{file: "minimax_think_test.go", keep: []string{"TestChatThinkTags", "TestMiniMaxThinkingGoesBackInTags"}},
		{file: "namespace_search_test.go", keep: []string{"TestResponsesCallArgsFromDone"}},
		{file: "namespace_test.go"},
		{file: "orphan_tool_output_test.go", keep: []string{"TestOrphanToolOutputsContentAndNoops"}},
		{file: "pair_tool_items_test.go", keep: []string{"TestPairToolItemsAnswersALoneCall", "inputOf"}},
		{file: "plain_schema_test.go"},
		{file: "plugin_parity_test.go", keep: []string{"TestCommandCodePluginReasoning", "TestCursorPluginFast", "chatBody", "thoughtChat"}},
		{file: "prompt_cache_test.go", keep: []string{"TestAnthropicPromptCache", "TestChatUsageCache"}},
		{file: "refusal_accounts_test.go", keep: []string{"TestPolicyRefusalShapes", "bioPolicy"}},
		{file: "responses_allowed_tools_test.go"},
		{file: "responses_cache_write_test.go"},
		{file: "responses_include_test.go", keep: []string{"TestResponsesKeepsInclude"}},
		{file: "responses_metadata_test.go", keep: []string{"TestClientMetadataOnlyToResponses"}},
		{file: "search_call_ids_test.go", keep: []string{"TestToolSearchCallHasTscID"}},
		{file: "search_test.go", keep: []string{"TestAnthropicServerToolLeftOut", "TestSearchToldAsAnthropics", "TestToolSearchReferenceIsTold"}},
		{file: "split_calls_test.go", keep: []string{
			"TestChatConsecutiveTextAssistantsUnchanged", "TestChatSplitCallsUnansweredStillSynthetic",
			"TestChatSplitParallelCallsKeepTheirResults", "TestChatSplitTextAfterCallKeepsResult",
			"TestResponsesSplitParallelCallsKeepTheirResults", "parseSplit", "splitCalls",
		}},
		{file: "standalone_review_test.go", keep: []string{"TestStandalonePlainEncoding"}},
		{file: "stop_context_test.go"},
		{file: "tool_images_test.go", keep: []string{"pngA", "pngB"}},
		{file: "tool_strict_test.go"},
		{file: "toolsearch_test.go", keep: []string{"TestToolSearchCallStreamedAndRendered", "TestToolSearchTranslated", "searchInput", "searchTools"}},
		{file: "translate_stream_test.go"},
	}},
	{from: "internal/provider", to: "llmconv/internal/provider", pkg: "provider", sources: []source{
		{file: "provider.go", keep: []string{"Protocol", "Chat"}},
		{file: "google.go", keep: []string{"CodeAssist"}},
		{file: "grok.go", keep: []string{"FlatName", "ObjectRoot", "objectRootIn", "nestedUnion", "grokRef", "appendDistinct"}},
		{file: "groupfast.go", keep: []string{"ClaudeFast", "claudeFastModels", "claudeDated"}},
		{file: "commandcode_plan.go", keep: []string{"CommandCodePlanID"}},
		{file: "grok_test.go", keep: []string{"TestObjectRootKeepsOwnRequired", "TestObjectRootNestedUnion"}},
	}, data: []string{"testdata/codex_automation_update_schema.json"}},
	{from: "internal/catalog", to: "llmconv/internal/catalog", pkg: "catalog", sources: []source{
		{file: "catalog.go", keep: []string{"DrawsID"}},
	}},
}

// localImport is where an upstream package's declarations were put, and ""
// for one that is not carried over.
func localImport(p string) string {
	for _, t := range targets {
		if p == upstreamModule+"/"+t.from {
			return localModule + "/" + t.to
		}
	}
	return ""
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sync /path/to/magpie")
		os.Exit(2)
	}
	up := os.Args[1]
	rev, err := exec.Command("git", "-C", up, "rev-parse", "HEAD").Output()
	if err != nil {
		fatal(fmt.Errorf("read upstream revision: %w", err))
	}
	commit := strings.TrimSpace(string(rev))
	// data files carry no header to know them by: the last run listed the
	// ones it copied, and one dropped from the list must not linger either
	if old, err := os.ReadFile(upstreamFile); err == nil {
		for _, line := range strings.Split(string(old), "\n")[1:] {
			if f := filepath.Clean(line); line != "" && strings.HasPrefix(f, "llmconv"+string(filepath.Separator)) {
				os.Remove(f)
				os.Remove(filepath.Dir(f)) // when that left it empty
			}
		}
	}
	var copied []string
	for _, t := range targets {
		if err := os.MkdirAll(t.to, 0o755); err != nil {
			fatal(err)
		}
		// a file dropped from the list must not linger
		old, _ := filepath.Glob(filepath.Join(t.to, "*.go"))
		for _, f := range old {
			if b, _ := os.ReadFile(f); bytes.HasPrefix(b, []byte(generatedMark)) {
				os.Remove(f)
			}
		}
		for _, s := range t.sources {
			out, err := generate(filepath.Join(up, t.from, s.file), t.pkg, s, commit)
			if err != nil {
				fatal(fmt.Errorf("%s/%s: %w", t.from, s.file, err))
			}
			if err := os.WriteFile(filepath.Join(t.to, s.file), out, 0o644); err != nil {
				fatal(err)
			}
		}
		for _, d := range t.data {
			b, err := os.ReadFile(filepath.Join(up, t.from, d))
			if err != nil {
				fatal(err)
			}
			to := filepath.Join(t.to, d)
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				fatal(err)
			}
			if err := os.WriteFile(to, b, 0o644); err != nil {
				fatal(err)
			}
			copied = append(copied, filepath.ToSlash(to))
		}
	}
	// the revision, then the data files copied from it
	list := upstreamModule + " " + commit + "\n"
	for _, f := range copied {
		list += f + "\n"
	}
	if err := os.WriteFile(upstreamFile, []byte(list), 0o644); err != nil {
		fatal(err)
	}
}

const upstreamFile = "llmconv/UPSTREAM"

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "sync:", err)
	os.Exit(1)
}

const generatedMark = "// Code generated by llmconv/internal/sync from "

// names are what a declaration can be asked for by: a function's name, a
// method's receiver type and Type.method, each name a var, const or type
// declaration gives.
func names(d ast.Decl) []string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return []string{d.Name.Name}
		}
		t := d.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if ix, ok := t.(*ast.IndexExpr); ok {
			t = ix.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return []string{id.Name, id.Name + "." + d.Name.Name}
		}
	case *ast.GenDecl:
		var out []string
		for _, sp := range d.Specs {
			switch sp := sp.(type) {
			case *ast.TypeSpec:
				out = append(out, sp.Name.Name)
			case *ast.ValueSpec:
				for _, n := range sp.Names {
					out = append(out, n.Name)
				}
			}
		}
		return out
	}
	return nil
}

func generate(path, pkg string, s source, commit string) ([]byte, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range append(append([]string{}, s.keep...), s.drop...) {
		want[n] = false
	}
	offset := func(p token.Pos) int { return fset.Position(p).Offset }
	var kept []ast.Decl
	var body bytes.Buffer
	last := offset(f.Name.End()) // end of what was read of src
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			last = offset(d.End())
			continue
		}
		named := false
		for _, n := range names(d) {
			if _, ok := want[n]; ok {
				want[n], named = true, true
			}
		}
		start := offset(d.Pos())
		if doc := docOf(d); doc != nil {
			start = offset(doc.Pos())
		}
		take := len(s.keep) == 0 && !named || len(s.keep) > 0 && named
		if len(s.keep) == 0 {
			// what stands between declarations stays with a file taken whole
			body.Write(src[last:start])
		}
		if take {
			kept = append(kept, d)
			body.Write(src[start:offset(d.End())])
			body.WriteString("\n\n")
		}
		last = offset(d.End())
	}
	if len(s.keep) == 0 {
		body.Write(src[last:])
	}
	for n, found := range want {
		if !found {
			return nil, fmt.Errorf("no declaration named %s", n)
		}
	}

	used := map[string]bool{}
	for _, d := range kept {
		ast.Inspect(d, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					used[id.Name] = true
				}
			}
			return true
		})
	}
	var imports []string
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := importName(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		if name != "_" && !used[name] {
			continue
		}
		if strings.HasPrefix(p, upstreamModule+"/") {
			local := localImport(p)
			if local == "" {
				return nil, fmt.Errorf("still imports %s", p)
			}
			p = local
		}
		line := strconv.Quote(p)
		if im.Name != nil {
			line = im.Name.Name + " " + line
		}
		imports = append(imports, line)
	}
	sort.Strings(imports)

	var out bytes.Buffer
	fmt.Fprintf(&out, "%s%s@%s. DO NOT EDIT.\n\n", generatedMark, upstreamModule, commit)
	fmt.Fprintf(&out, "package %s\n\n", pkg)
	if len(imports) > 0 {
		out.WriteString("import (\n")
		for _, im := range imports {
			out.WriteString("\t" + im + "\n")
		}
		out.WriteString(")\n\n")
	}
	out.Write(body.Bytes())
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format: %w", err)
	}
	return formatted, nil
}

// importName is the name a package imported from p goes by with none given:
// the last element of its path, or the one before a major version.
func importName(p string) string {
	parts := strings.Split(p, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		return parts[len(parts)-2]
	}
	return last
}

func docOf(d ast.Decl) *ast.CommentGroup {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}
