// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package cursor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/elastic/go-concert/ctxtool"
	"github.com/elastic/go-concert/unison"

	input "github.com/elastic/beats/v7/filebeat/input/v2"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common/acker"
	"github.com/elastic/beats/v7/libbeat/monitoring/inputmon"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
)

// Input interface for cursor based inputs. This interface must be implemented
// by inputs that with to use the InputManager in order to implement a stateful
// input that can store state between restarts.
type Input interface {
	Name() string

	// Test checks the configuaration and runs additional checks if the Input can
	// actually collect data for the given configuration (e.g. check if host/port or files are
	// accessible).
	// The input manager will call Test per configured source.
	Test(Source, input.TestContext) error

	// Run starts the data collection. Run must return an error only if the
	// error is fatal making it impossible for the input to recover.
	// The input run a go-routine can call Run per configured Source.
	Run(input.Context, Source, Cursor, Publisher) error
}

// managedInput implements the v2.Input interface, integrating cursor Inputs
// with the v2 input API.
// The managedInput starts go-routines per configured source.
// If a Run returns the error is 'remembered', but active data collecting
// continues. Only after all Run calls have returned will the managedInput be
// done.
type managedInput struct {
	manager      *InputManager
	userID       string
	sources      []Source
	input        Input
	cleanTimeout time.Duration
}

// Name is required to implement the v2.Input interface
func (inp *managedInput) Name() string { return inp.input.Name() }

// Test runs the Test method for each configured source.
func (inp *managedInput) Test(ctx input.TestContext) error {
	var grp unison.MultiErrGroup
	for _, source := range inp.sources {
		source := source
		grp.Go(func() (err error) {
			return inp.testSource(ctx, source)
		})
	}

	errs := grp.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("input tests failed: %w", errors.Join(errs...))
	}
	return nil
}

// IsFIPSCapable returns true if the input is capable of running with
// FIPS-compliant algorithms; false, otherwise.
func (inp *managedInput) IsFIPSCapable() bool {
	if fipsAware, ok := inp.input.(input.FIPSAwareInput); ok {
		return fipsAware.IsFIPSCapable()
	}
	return true
}

func (inp *managedInput) testSource(ctx input.TestContext, source Source) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("input panic with: %+v\n%s", v, debug.Stack())
			ctx.Logger.Errorf("Input crashed with: %+v", err)
		}
	}()
	return inp.input.Test(source, ctx)
}

// Run creates a go-routine per source, waiting until all go-routines have
// returned, either by error, or by shutdown signal.
// If an input panics, we create an error value with stack trace to report the
// issue, but not crash the whole process.
func (inp *managedInput) Run(
	ctx input.Context,
	pipeline beat.PipelineConnector,
) (err error) {
	// Setup cancellation using a custom cancel context. All workers will be
	// stopped if one failed badly by returning an error.
	cancelCtx, cancel := context.WithCancel(ctxtool.FromCanceller(ctx.Cancelation))
	defer cancel()
	ctx.Cancelation = cancelCtx

	// Override the reported type in the registry to a sentinel value so that
	// metrics reporters that aggregate over inputs know to skip this registry
	// and report its nested sources (instantiated below) as top-level inputs.
	// (We need child inputs to be reported at the top level for backwards
	// compatibility, but we don't want to expose the top-level inputs registry
	// to every individual input, so we keep nested input metrics within their
	// parent's registry and aggregate them at the top level during the reporting
	// stage.)
	monitoring.NewString(ctx.MetricsRegistry, "input").Set(inputmon.InputNested)

	var grp unison.MultiErrGroup
	for _, source := range inp.sources {
		source := source
		grp.Go(func() (err error) {
			// refine per worker context
			inpCtxID := ctx.ID + "::" + source.Name()
			log := ctx.Logger.With("input_source", source.Name())

			reg, pc, cancelMetrics := input.PrepareInputMetrics(
				inpCtxID,
				ctx.Name,
				ctx.MetricsRegistry,
				pipeline,
				log)
			defer cancelMetrics()

			inpCtx := input.Context{
				ID:              inpCtxID,
				IDWithoutName:   ctx.ID, // Preserve IDWithoutName, in case the context was constructed who knows how
				Name:            ctx.Name,
				Agent:           ctx.Agent,
				Cancelation:     ctx.Cancelation,
				StatusReporter:  ctx.StatusReporter,
				MetricsRegistry: reg,
				Logger:          log,
			}

			if err = inp.runSource(inpCtx, inp.manager.store, source, pc); err != nil {
				cancel()
			}
			return err
		})
	}

	if errs := grp.Wait(); len(errs) > 0 {
		return fmt.Errorf("input %s failed: %w", ctx.ID, errors.Join(errs...))
	}
	return nil
}

func (inp *managedInput) runSource(
	ctx input.Context,
	store *store,
	source Source,
	pipeline beat.PipelineConnector,
) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("input panic with: %+v\n%s", v, debug.Stack())
			ctx.Logger.Errorf("Input crashed with: %+v", err)
		}
	}()

	client, err := pipeline.ConnectWith(beat.ClientConfig{
		EventListener: newInputACKHandler(ctx.Logger),
	})
	if err != nil {
		return err
	}
	defer client.Close()

	resourceKey := inp.createSourceID(source)
	resource, err := inp.manager.lock(ctx, resourceKey)
	if err != nil {
		return err
	}
	defer releaseResource(resource)

	store.UpdateTTL(resource, inp.cleanTimeout)

	cursor := makeCursor(store, resource)
	publisher := &cursorPublisher{canceler: ctx.Cancelation, client: client, cursor: &cursor}
	return inp.input.Run(ctx, source, cursor, publisher)
}

func (inp *managedInput) createSourceID(s Source) string {
	if inp.userID != "" {
		return fmt.Sprintf("%v::%v::%v", inp.manager.Type, inp.userID, s.Name())
	}
	return fmt.Sprintf("%v::%v", inp.manager.Type, s.Name())
}

func newInputACKHandler(log *logp.Logger) beat.EventListener {
	return acker.EventPrivateReporter(func(acked int, private []interface{}) {
		var n uint
		var last int
		for i := 0; i < len(private); i++ {
			current := private[i]
			if current == nil {
				continue
			}

			if _, ok := current.(*updateOp); !ok {
				continue
			}

			n++
			last = i
		}

		if n == 0 {
			return
		}
		private[last].(*updateOp).Execute(n)
	})
}
