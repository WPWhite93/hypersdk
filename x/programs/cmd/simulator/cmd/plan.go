// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamensky/argparse"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/logging"
	"go.uber.org/zap"

	"github.com/ava-labs/hypersdk/codec"
	"github.com/ava-labs/hypersdk/state"
	"github.com/ava-labs/hypersdk/x/programs/cmd/simulator/vm/storage"
	"github.com/ava-labs/hypersdk/x/programs/cmd/simulator/vm/utils"
)

var _ Cmd = (*runCmd)(nil)

type runCmd struct {
	cmd *argparse.Command

	lastStep *int
	file     *string
	planStep *string

	step   *Step
	log    logging.Logger
	reader *bufio.Reader

	// tracks program IDs created during this simulation
	programIDStrMap map[int]ids.ID
}

func (c *runCmd) New(parser *argparse.Parser, programIDStrMap map[int]ids.ID, lastStep *int, reader *bufio.Reader) {
	c.programIDStrMap = programIDStrMap
	c.cmd = parser.NewCommand("run", "Run a HyperSDK program simulation plan")
	c.file = c.cmd.String("", "file", &argparse.Options{
		Required: false,
	})
	c.planStep = c.cmd.String("", "step", &argparse.Options{
		Required: false,
	})
	c.lastStep = lastStep
	c.reader = reader
}

func (c *runCmd) Run(ctx context.Context, log logging.Logger, db *state.SimpleMutable, _ []string) (*Response, error) {
	c.log = log
	var err error
	if err = c.Init(); err != nil {
		return newResponse(0), err
	}
	if err = c.Verify(); err != nil {
		return newResponse(0), err
	}
	resp, err := c.RunStep(ctx, db)
	if err != nil {
		return newResponse(0), err
	}
	return resp, nil
}

func (c *runCmd) Happened() bool {
	return c.cmd.Happened()
}

func (c *runCmd) Init() (err error) {
	var planStep []byte
	switch {
	case c.planStep != nil && len(*c.planStep) > 0:
		{
			planStep = []byte(*c.planStep)
		}
	case len(*c.file) > 0:
		{
			// read simulation step from file
			planStep, err = os.ReadFile(*c.file)
			if err != nil {
				return err
			}
		}
	default:
		return errors.New("please specify either a --plan or a --file flag")
	}

	c.step, err = unmarshalStep(planStep)
	if err != nil {
		return err
	}

	return nil
}

func (c *runCmd) Verify() error {
	step := c.step
	if step == nil {
		return fmt.Errorf("%w: %s", ErrInvalidPlan, "no steps found")
	}

	if step.Params == nil {
		return fmt.Errorf("%w: %s", ErrInvalidStep, "no params found")
	}

	// verify endpoint requirements
	return verifyEndpoint(*c.lastStep, step)
}

func verifyEndpoint(i int, step *Step) error {
	firstParamType := step.Params[0].Type

	switch step.Endpoint {
	case EndpointKey:
		// verify the first param is a string for key name
		if firstParamType != KeyEd25519 && firstParamType != KeySecp256k1 {
			return fmt.Errorf("%w %d %w: expected ed25519 or secp256k1", ErrInvalidStep, i, ErrInvalidParamType)
		}
	case EndpointReadOnly:
		// verify the first param is a program ID
		if firstParamType != ID {
			return fmt.Errorf("%w %d %w: %w", ErrInvalidStep, i, ErrInvalidParamType, ErrFirstParamRequiredID)
		}
	case EndpointExecute:
		if step.Method == ProgramCreate {
			// verify the first param is a string for the path
			if step.Params[0].Type != String {
				return fmt.Errorf("%w %d %w: %w", ErrInvalidStep, i, ErrInvalidParamType, ErrFirstParamRequiredString)
			}
		} else {
			// verify the first param is a program id
			if step.Params[0].Type != ID {
				return fmt.Errorf("%w %d %w: %w", ErrInvalidStep, i, ErrInvalidParamType, ErrFirstParamRequiredID)
			}
		}
	default:
		return fmt.Errorf("%w: %s", ErrInvalidEndpoint, step.Endpoint)
	}
	return nil
}

func (c *runCmd) RunStep(ctx context.Context, db *state.SimpleMutable) (*Response, error) {
	index := *c.lastStep
	step := c.step
	c.log.Info("simulation",
		zap.Int("step", index),
		zap.String("endpoint", string(step.Endpoint)),
		zap.String("method", step.Method),
		zap.Uint64("maxUnits", step.MaxUnits),
		zap.Any("params", step.Params),
	)

	params, err := c.createCallParams(ctx, db, step.Params, step.Endpoint)
	if err != nil {
		c.log.Error(fmt.Sprintf("simulation call: %s", err))
		return newResponse(0), err
	}

	resp := newResponse(index)
	err = runStepFunc(ctx, c.log, db, step.Endpoint, step.MaxUnits, step.Method, params, resp)
	if err != nil {
		resp.setError(err)
	}

	// map all transactions to their step_N identifier
	txID, found := resp.getTxID()
	if found {
		id, err := ids.FromString(txID)
		if err != nil {
			return nil, err
		}
		c.programIDStrMap[index] = id
	}

	lastStep := index + 1
	*c.lastStep = lastStep

	return resp, nil
}

func runStepFunc(
	ctx context.Context,
	log logging.Logger,
	db *state.SimpleMutable,
	endpoint Endpoint,
	maxUnits uint64,
	method string,
	params []Parameter,
	resp *Response,
) error {
	defer resp.setTimestamp(time.Now().Unix())
	switch endpoint {
	case EndpointKey:
		keyName := string(params[0].Value)
		key, err := keyCreateFunc(ctx, db, keyName)
		if errors.Is(err, ErrDuplicateKeyName) {
			log.Debug("key already exists")
		} else if err != nil {
			return err
		}
		resp.setMsg("created named key with address " + utils.Address(key))

		return nil
	case EndpointExecute: // for now the logic is the same for both TODO: breakout readonly
		if method == ProgramCreate {
			// get program path from params
			programPath := string(params[0].Value)
			id, err := programCreateFunc(ctx, db, programPath)
			if err != nil {
				return err
			}
			resp.setTxID(id.String())
			resp.setTimestamp(time.Now().Unix())

			return nil
		}
		id, err := ids.ToID(params[0].Value)
		if err != nil {
			return err
		}
		id, response, balance, err := programExecuteFunc(ctx, log, db, id, params[1:], method, maxUnits)
		if err != nil {
			return err
		}

		if len(response) > 1 {
			return errors.New("multi response not supported")
		}
		res := response[0]
		if res != nil {
			resp.setResponse(res)
		}
		resp.setTxID(id.String())
		resp.setBalance(balance)

		return nil
	case EndpointReadOnly:
		id, err := ids.ToID(params[0].Value)
		if err != nil {
			return err
		}
		// TODO: implement readonly for now just don't charge for gas
		_, response, _, err := programExecuteFunc(ctx, log, db, id, params[1:], method, math.MaxUint64)
		if err != nil {
			return err
		}

		if len(response) > 1 {
			return errors.New("multi response not supported")
		}
		res := response[0]
		if res != nil {
			resp.setResponse(res)
		}

		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInvalidEndpoint, endpoint)
	}
}

// createCallParams converts a slice of Parameters to a slice of runtime.CallParams.
func (c *runCmd) createCallParams(ctx context.Context, db state.Immutable, params []Parameter, endpoint Endpoint) ([]Parameter, error) {
	cp := make([]Parameter, 0, len(params))
	for _, param := range params {
		switch param.Type {
		case String, ID:
			stepIDStr := string(param.Value)
			idString, found := strings.CutPrefix(stepIDStr, "step_")
			if found {
				id, err := strconv.ParseInt(idString, 10, 32)
				if err != nil {
					return nil, err
				}
				programID, ok := c.programIDStrMap[int(id)]
				if !ok {
					return nil, fmt.Errorf("failed to map to id: %s", stepIDStr)
				}
				cp = append(cp, Parameter{Value: programID[:], Type: param.Type})
			} else {
				programID, err := ids.ToID(param.Value)
				if err == nil {
					cp = append(cp, Parameter{Value: programID[:], Type: param.Type})
				} else {
					path := string(param.Value)
					if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
						return nil, errors.New("this path does not exists")
					}
					cp = append(cp, param)
				}
			}
		case KeyEd25519: // TODO: support secp256k1
			key := string(param.Value)
			// get named public key from db
			pk, ok, err := storage.GetPublicKey(ctx, db, key)
			if err != nil {
				return nil, err
			}
			if !ok && endpoint != EndpointKey {
				// using not stored named public key in other context than key creation
				return nil, fmt.Errorf("%w: %s", ErrNamedKeyNotFound, key)
			}
			if ok {
				// otherwise use the public key address
				address := make([]byte, codec.AddressLen)
				address[0] = 0 // prefix
				copy(address[1:], pk[:])
				key = string(address)
			}
			if err != nil {
				return nil, err
			}
			cp = append(cp, Parameter{Value: []byte(key), Type: param.Type})
		case Uint64, Bool:
			cp = append(cp, param)
		default:
			return nil, fmt.Errorf("%w: %s", ErrInvalidParamType, param.Type)
		}
	}

	return cp, nil
}

// generateRandomID creates a unique ID.
// Note: ids.GenerateID() is not used because the IDs are not unique and will
// collide.
func generateRandomID() (ids.ID, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	if err != nil {
		return ids.Empty, err
	}
	id, err := ids.ToID(key)
	if err != nil {
		return ids.Empty, err
	}

	return id, nil
}
