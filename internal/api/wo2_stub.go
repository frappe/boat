package api

import (
	"context"
	"errors"

	"github.com/frappe/boat/internal/wire"
)

// STUB: the WO-2 verb surface, so the repo compiles while it is written.
// Replace each with a real handler and delete this file when it empties.
//
// Every replacement goes through server.perform, which is where the claim, the
// VM's turn and the terminal journal record all live. A handler that reaches
// server.virtualMachines on its own is a second driver of the host: it can run
// while a reconcile pass is mid-boot, and it leaves no record behind.

var errVerbNotImplemented = errors.New("not implemented")

func (server *Server) PauseVirtualMachine(ctx context.Context, request wire.PauseVirtualMachineRequestObject) (wire.PauseVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) ResumeVirtualMachine(ctx context.Context, request wire.ResumeVirtualMachineRequestObject) (wire.ResumeVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) SleepVirtualMachine(ctx context.Context, request wire.SleepVirtualMachineRequestObject) (wire.SleepVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) WakeVirtualMachine(ctx context.Context, request wire.WakeVirtualMachineRequestObject) (wire.WakeVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) RebuildVirtualMachine(ctx context.Context, request wire.RebuildVirtualMachineRequestObject) (wire.RebuildVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) TerminateVirtualMachine(ctx context.Context, request wire.TerminateVirtualMachineRequestObject) (wire.TerminateVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}

func (server *Server) ResizeVirtualMachine(ctx context.Context, request wire.ResizeVirtualMachineRequestObject) (wire.ResizeVirtualMachineResponseObject, error) {
	return nil, errVerbNotImplemented
}
