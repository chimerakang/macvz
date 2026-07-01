package linuxpod

import (
	"encoding/json"
	"errors"
	"fmt"
)

// protocol.go defines the newline-delimited JSON (NDJSON) wire protocol the Go
// HelperClient and the Swift helper both speak over a stream connection (a unix
// socket in production, an in-memory pipe in tests). One request is one JSON
// object on a line; one response is one JSON object on a line. NDJSON is chosen
// over gRPC/protobuf so the helper stub stays a few hundred lines of Foundation
// Swift with no code generation, while remaining trivially fakeable in Go.
//
// The op names mirror the Backend methods exactly so the dispatch table and the
// Swift helper read one-to-one against the contract.

// Op identifies a backend method on the wire.
type Op string

const (
	opPing            Op = "Ping"
	opAdopt           Op = "Adopt"
	opCreatePod       Op = "CreatePod"
	opPodStatus       Op = "PodStatus"
	opPrepareRootfs   Op = "PrepareContainerRootfs"
	opCreateContainer Op = "CreateContainer"
	opStartContainer  Op = "StartContainer"
	opStopContainer   Op = "StopContainer"
	opRemoveContainer Op = "RemoveContainer"
	opStatus          Op = "Status"
	opCleanup         Op = "Cleanup"
	opContainerLog    Op = "ContainerLogPath"
	opExecSync        Op = "ExecSync"
	opExecStream      Op = "ExecStream"
	opContainerStats  Op = "ContainerStats"
	opAttach          Op = "Attach"
	opPortForward     Op = "PortForward"
)

// wireRequest is the envelope for one call. Payload is the op-specific request
// struct (PodSpec, RootfsRequest, CreateRequest, StopRequest, Ref, or {podID}).
type wireRequest struct {
	Op      Op              `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// wireResponse is the envelope for one reply. On success OK is true and Result
// holds the op-specific response; on failure OK is false and Error/Code describe
// it. Code carries a stable error class so the client maps it back to a sentinel
// rather than matching error text.
type wireResponse struct {
	OK     bool            `json:"ok"`
	Code   errCode         `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// errCode is the stable wire classification for a backend error.
type errCode string

const (
	codeNone               errCode = ""
	codePodNotFound        errCode = "PodNotFound"
	codeContainerNotFound  errCode = "ContainerNotFound"
	codeRootfsNotFound     errCode = "RootfsNotFound"
	codeInvalid            errCode = "Invalid"
	codeAlreadyExists      errCode = "AlreadyExists"
	codeIdentityUnverified errCode = "IdentityUnverified"
	codeUnsupported        errCode = "Unsupported"
	codeInternal           errCode = "Internal"
)

// codeForError maps a backend error to its wire code. Unknown errors map to
// Internal so they still round-trip with their message.
func codeForError(err error) errCode {
	switch {
	case err == nil:
		return codeNone
	case errors.Is(err, ErrPodNotFound):
		return codePodNotFound
	case errors.Is(err, ErrContainerNotFound):
		return codeContainerNotFound
	case errors.Is(err, ErrRootfsNotFound):
		return codeRootfsNotFound
	case errors.Is(err, ErrAlreadyExists):
		return codeAlreadyExists
	case errors.Is(err, ErrInvalid):
		return codeInvalid
	case errors.Is(err, ErrIdentityUnverified):
		return codeIdentityUnverified
	case errors.Is(err, ErrUnsupported):
		return codeUnsupported
	default:
		return codeInternal
	}
}

// errorForCode reconstructs a client-side error from a wire code and message. It
// wraps the matching sentinel so callers can use errors.Is, preserving the
// helper's message as detail.
func errorForCode(code errCode, msg string) error {
	if msg == "" {
		msg = string(code)
	}
	switch code {
	case codePodNotFound:
		return fmt.Errorf("%w: %s", ErrPodNotFound, msg)
	case codeContainerNotFound:
		return fmt.Errorf("%w: %s", ErrContainerNotFound, msg)
	case codeRootfsNotFound:
		return fmt.Errorf("%w: %s", ErrRootfsNotFound, msg)
	case codeInvalid:
		return fmt.Errorf("%w: %s", ErrInvalid, msg)
	case codeAlreadyExists:
		return fmt.Errorf("%w: %s", ErrAlreadyExists, msg)
	case codeIdentityUnverified:
		return fmt.Errorf("%w: %s", ErrIdentityUnverified, msg)
	case codeUnsupported:
		return fmt.Errorf("%w: %s", ErrUnsupported, msg)
	default:
		return errors.New(msg)
	}
}

// mustRaw marshals v to json.RawMessage; it only errors on unencodable values,
// which the fixed contract types never contain.
func mustRaw(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("linuxpod: encode payload: %w", err)
	}
	return b, nil
}
