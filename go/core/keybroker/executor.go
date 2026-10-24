package keybroker

import (
	"context"
	"errors"
)

type commandOperation uint8

const (
	commandRead commandOperation = iota + 1
	commandReadLatest
	commandAdd
)

type securityCommand struct {
	operation commandOperation
	account   string
	value     []byte
}

type securityResult struct {
	value   []byte
	version Version
}

type commandExecutor interface {
	execute(context.Context, *securityCommand) (securityResult, error)
	close() error
}

var (
	errEntryNotFound = errors.New("keybroker entry not found")
	errEntryExists   = errors.New("keybroker entry already exists")
)
