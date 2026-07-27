package api

import (
	"context"
	"errors"

	"github.com/frappe/boat/internal/wire"
)

// STUB: the WO-1 surface, so the repo compiles while it is written.
// Replace each of these with a real handler; delete this file when it empties.

var errNotImplemented = errors.New("not implemented")

func (server *Server) GetExport(
	ctx context.Context, request wire.GetExportRequestObject,
) (wire.GetExportResponseObject, error) {
	return nil, errNotImplemented
}

func (server *Server) PutVirtualMachine(
	ctx context.Context, request wire.PutVirtualMachineRequestObject,
) (wire.PutVirtualMachineResponseObject, error) {
	return nil, errNotImplemented
}

func (server *Server) Watch(
	ctx context.Context, request wire.WatchRequestObject,
) (wire.WatchResponseObject, error) {
	return nil, errNotImplemented
}
