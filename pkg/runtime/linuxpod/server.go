package linuxpod

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
)

// server.go is the helper-side of the protocol: it reads NDJSON requests off a
// connection, dispatches them to a Backend, and writes NDJSON responses. The
// Swift helper implements the same dispatch; the Go Serve here lets tests back a
// HelperClient with the in-process FakeBackend over net.Pipe, proving the wire
// protocol round-trips every op without a socket or Swift toolchain.

// Serve handles requests on conn against backend until the peer closes the
// connection or the context is cancelled. One connection carries a sequence of
// request lines; each is answered with exactly one response line, in order. A
// malformed request line is answered with an Invalid error rather than dropping
// the connection. Serve closes conn before returning.
func Serve(ctx context.Context, conn net.Conn, backend Backend) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if werr := writeResponse(conn, dispatch(ctx, backend, line)); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// dispatch decodes one request line and invokes the matching backend method,
// returning the response envelope (success or classified error).
func dispatch(ctx context.Context, backend Backend, line []byte) wireResponse {
	var req wireRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errResponse(ErrInvalid)
	}
	switch req.Op {
	case opPing:
		return result(backend.Ping(ctx))
	case opCreatePod:
		var spec PodSpec
		if err := decode(req.Payload, &spec); err != nil {
			return errResponse(err)
		}
		return result(backend.CreatePod(ctx, spec))
	case opPrepareRootfs:
		var r RootfsRequest
		if err := decode(req.Payload, &r); err != nil {
			return errResponse(err)
		}
		return result(backend.PrepareContainerRootfs(ctx, r))
	case opCreateContainer:
		var r CreateRequest
		if err := decode(req.Payload, &r); err != nil {
			return errResponse(err)
		}
		return result(backend.CreateContainer(ctx, r))
	case opStartContainer:
		var ref Ref
		if err := decode(req.Payload, &ref); err != nil {
			return errResponse(err)
		}
		return result(backend.StartContainer(ctx, ref))
	case opStopContainer:
		var r StopRequest
		if err := decode(req.Payload, &r); err != nil {
			return errResponse(err)
		}
		return result(backend.StopContainer(ctx, r))
	case opRemoveContainer:
		var ref Ref
		if err := decode(req.Payload, &ref); err != nil {
			return errResponse(err)
		}
		if err := backend.RemoveContainer(ctx, ref); err != nil {
			return errResponse(err)
		}
		return wireResponse{OK: true}
	case opStatus:
		var ref Ref
		if err := decode(req.Payload, &ref); err != nil {
			return errResponse(err)
		}
		return result(backend.Status(ctx, ref))
	case opCleanup:
		var p struct {
			PodID string `json:"podID"`
		}
		if err := decode(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		return result(backend.Cleanup(ctx, p.PodID))
	default:
		return errResponse(ErrInvalid)
	}
}

// decode unmarshals a payload, mapping any failure to ErrInvalid so a malformed
// request is reported as a client fault, not an internal error.
func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ErrInvalid
	}
	return nil
}

// result wraps a (value, error) backend return into a response envelope.
func result[T any](v T, err error) wireResponse {
	if err != nil {
		return errResponse(err)
	}
	raw, merr := mustRaw(v)
	if merr != nil {
		return errResponse(merr)
	}
	return wireResponse{OK: true, Result: raw}
}

// errResponse classifies err into a wire response.
func errResponse(err error) wireResponse {
	return wireResponse{OK: false, Code: codeForError(err), Error: err.Error()}
}

// writeResponse marshals resp and writes it as one NDJSON line.
func writeResponse(w io.Writer, resp wireResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		// Last-resort: a response that cannot be marshalled is itself an internal
		// error the peer should see, not a dropped connection.
		b, _ = json.Marshal(wireResponse{OK: false, Code: codeInternal, Error: "encode response: " + err.Error()})
	}
	b = append(b, '\n')
	_, werr := w.Write(b)
	return werr
}
