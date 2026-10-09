// Package llmconv converts requests and replies between four LLM wire
// protocols: OpenAI Chat Completions, OpenAI Responses, Anthropic Messages
// and Google Gemini generateContent.
//
// Every conversion goes through one intermediate form. A request body is
// parsed into a [Request] and built again for another protocol. A reply is
// read as a stream of [Event] values, which an [Encoder] writes back out as
// another protocol's server-sent events, or a [Collector] gathers into a
// [Result] that [Render] turns into a whole, non-streamed reply body.
//
// Replies are only read as streams: there is no parser for a non-streamed
// upstream reply body. A caller serving a non-streaming client asks its
// upstream to stream (set Request.Stream before Build) and uses
// [ConvertResponse] to collect the stream into one body.
//
// Most of this package is generated from the gateway of
// github.com/yetone/magpie (see UPSTREAM and internal/sync); the files
// without a "Code generated" header are written by hand.
package llmconv

import (
	"fmt"
	"io"
	"net/http"

	"github.com/aisk/cameo/llmconv/internal/provider"
)

// Protocol names a wire protocol.
type Protocol = provider.Protocol

const (
	Chat      = provider.Chat      // OpenAI Chat Completions
	Responses = provider.Responses // OpenAI Responses
	Anthropic = provider.Anthropic // Anthropic Messages
	Gemini    = provider.Gemini    // Google Gemini generateContent
)

func known(proto Protocol) error {
	switch proto {
	case Chat, Responses, Anthropic, Gemini:
		return nil
	}
	return fmt.Errorf("llmconv: unknown protocol %q", proto)
}

// BuildOptions says who a request is built for. The zero value builds a
// plain request with no vendor-specific shaping.
type BuildOptions struct {
	// Host is the host name of the upstream the request will be sent to,
	// such as "api.openai.com" or "api.deepseek.com". The builders shape
	// the body for vendors they recognise by it:
	//
	//   - Chat: "max_completion_tokens" instead of "max_tokens" on
	//     *openai.com; reasoning text replayed on earlier assistant turns
	//     for DeepSeek; thinking put back inline in <think> tags for
	//     MiniMax; thought signatures on tool calls for Gemini's
	//     OpenAI-compatible endpoint; an assistant prefill marked as
	//     DeepSeek's "prefix" or Moonshot's "partial"; OpenRouter's web
	//     plugin when the request asks for web search.
	//   - Responses: service_tier "priority" for a fast request only on
	//     api.openai.com and chatgpt.com; reasoning text replayed for
	//     DeepSeek models served elsewhere; the include entry for web
	//     search sources left out on api.x.ai.
	//   - Anthropic: "speed": "fast" for a fast request, only on
	//     api.anthropic.com and only for models that have fast mode.
	//   - Gemini: not used.
	//
	// The model name is consulted alongside it for some of these.
	Host string

	// RejectTemperature leaves "temperature" and "top_p" out of a Chat or
	// Responses body, for upstream models that refuse sampling parameters
	// (OpenAI's reasoning models, for one). The Anthropic and Gemini
	// builders ignore it.
	RejectTemperature bool
}

// Parse reads a request body of the given protocol.
//
// A Gemini request names its model and whether it streams in the URL. Parse
// reads top-level "model" and "stream" fields from a Gemini body that has
// them and otherwise leaves Request.Model and Request.Stream for the caller
// to set.
func Parse(proto Protocol, body []byte) (*Request, error) {
	if err := known(proto); err != nil {
		return nil, err
	}
	return parse(proto, body)
}

// Build renders r as a request body of the given protocol, asking for
// model. r.Stream decides whether the body asks for a streamed reply, except
// for Gemini, where the URL does.
func Build(proto Protocol, r *Request, model string, opts BuildOptions) ([]byte, error) {
	if err := known(proto); err != nil {
		return nil, err
	}
	return build(proto, r, model, opts.Host, opts.RejectTemperature)
}

// ConvertRequest turns a request body of protocol from into one of protocol
// to. model is the model the new body asks for; when it is empty the model
// the original body named is kept.
func ConvertRequest(from, to Protocol, body []byte, model string, opts BuildOptions) ([]byte, error) {
	r, err := Parse(from, body)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = r.Model
	}
	return Build(to, r, model, opts)
}

// ReadSSE walks a server-sent event stream, calling fn with the name and
// data of each event. Data lines of one event are joined with newlines and
// comment lines are skipped. It stops at the first error fn returns and
// returns it.
func ReadSSE(r io.Reader, fn func(event, data string) error) error {
	return readSSE(r, fn)
}

// Decoder reads the streamed reply of one protocol as events. It keeps state
// across calls, so each reply needs its own.
type Decoder struct {
	decode func(data string, emit func(Event)) error
}

// NewDecoder returns a decoder for a reply streamed in proto. A protocol
// that is not one of the four is read as Anthropic.
func NewDecoder(proto Protocol) *Decoder {
	return &Decoder{decode: decoder(proto)}
}

// Decode reads the data of one server-sent event, as [ReadSSE] gives it,
// and calls emit for each thing it said. One event's data may say several
// things, or nothing.
func (d *Decoder) Decode(data string, emit func(Event)) error {
	return d.decode(data, emit)
}

// Encoder writes events to a client as the server-sent events of one
// protocol. The response's headers and status are written with the first
// event, and each event is flushed when w is an [http.Flusher].
type Encoder struct {
	enc streamEncoder
}

// NewEncoder returns an encoder writing a reply in proto to w. r is the
// request the reply answers, as the client sent it: its model is the one
// the reply names, and a Responses client that grouped its tools in
// namespaces has its calls named that way again. It may be nil. A protocol
// that is not one of the four is written as Anthropic.
func NewEncoder(proto Protocol, w http.ResponseWriter, r *Request) *Encoder {
	if r == nil {
		r = &Request{}
	}
	return &Encoder{enc: makeEncoder(proto, newSSEWriter(w), r)}
}

// Event writes what one event says, in the client's protocol.
func (e *Encoder) Event(ev Event) { e.enc.event(ev) }

// Finish ends the reply: open blocks are closed and the protocol's closing
// events are written. A reply that failed with a KError event is already
// ended by it and is not finished.
func (e *Encoder) Finish() { e.enc.finish() }

// Keepalive tells the client the reply is still going while there is
// nothing to say, in the way its protocol's idle timeout counts.
func (e *Encoder) Keepalive() { e.enc.keepalive() }

// UpstreamError is a failure an upstream reported inside its reply.
type UpstreamError struct {
	Status  int    // the HTTP status the error stood for, when it said
	Code    string // the upstream's error or safety-filter code, when it said
	Message string
}

func (e *UpstreamError) Error() string { return e.Message }

// Collector gathers the events of one reply into a [Result]. The zero value
// is ready to use.
type Collector struct {
	c collector
}

// Add takes one event of the reply.
func (c *Collector) Add(ev Event) { c.c.add(ev) }

// Err is the failure the reply reported with a KError event, as an
// [*UpstreamError], and nil when it reported none.
func (c *Collector) Err() error {
	if c.c.err == "" {
		return nil
	}
	return &UpstreamError{Status: c.c.errStatus, Code: c.c.errCode, Message: c.c.err}
}

// Result is the reply gathered so far, closed as a whole reply: a reply of
// tool calls with no stop reason of its own stops for them.
func (c *Collector) Result() Result { return c.c.finish() }

// Render writes a whole reply as a non-streamed response body of proto. r is
// the request the reply answers, as for [NewEncoder], and may be nil.
func Render(proto Protocol, res Result, r *Request) []byte {
	if r == nil {
		r = &Request{}
	}
	return render(proto, res, r)
}

// ConvertStream reads upstream, the server-sent event body of a reply
// streamed in protocol from, and writes it to w as a reply streamed in
// protocol to. r is the client's request, as for [NewEncoder].
//
// A reply that ends well is finished and nil is returned. When the upstream
// reports a failure inside the reply it is passed on to the client in its
// own protocol and returned as an [*UpstreamError]; when the stream cannot
// be read or decoded the client is told the same way and the read error is
// returned. Either way the client has been answered and w must not be
// written to again.
func ConvertStream(from, to Protocol, upstream io.Reader, w http.ResponseWriter, r *Request) error {
	if err := known(from); err != nil {
		return err
	}
	if err := known(to); err != nil {
		return err
	}
	dec, enc := NewDecoder(from), NewEncoder(to, w, r)
	var failed *UpstreamError
	err := ReadSSE(upstream, func(_, data string) error {
		return dec.Decode(data, func(ev Event) {
			if failed != nil {
				// an error ends the reply: nothing goes on after it
				return
			}
			if ev.Kind == KError {
				failed = &UpstreamError{Status: ev.Status, Code: ev.Code, Message: ev.Text}
			}
			enc.Event(ev)
		})
	})
	switch {
	case failed != nil:
		return failed
	case err != nil:
		enc.Event(Event{Kind: KError, Text: err.Error()})
		return err
	}
	enc.Finish()
	return nil
}

// ConvertResponse reads upstream as [ConvertStream] does and returns the
// whole reply as a non-streamed response body of protocol to, for a client
// that did not ask for a stream. The upstream still has to be asked to
// stream.
//
// A stream that cannot be read to its end is an error, since part of an
// answer is not an answer. A failure the upstream reported is returned as
// an [*UpstreamError] unless the reply had already said something, in which
// case what it said is rendered.
func ConvertResponse(from, to Protocol, upstream io.Reader, r *Request) ([]byte, error) {
	if err := known(from); err != nil {
		return nil, err
	}
	if err := known(to); err != nil {
		return nil, err
	}
	dec := NewDecoder(from)
	var col Collector
	if err := ReadSSE(upstream, func(_, data string) error {
		return dec.Decode(data, col.Add)
	}); err != nil {
		return nil, err
	}
	if err := col.Err(); err != nil && !saidAnything(col.c.res.Parts) {
		return nil, err
	}
	return Render(to, col.Result(), r), nil
}
