// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package engine

import (
	"github.com/juju/juju/agent"
	"github.com/juju/juju/api/base"
	"github.com/juju/juju/worker"
	"github.com/juju/juju/worker/dependency"
)

// Many manifolds completely depend on an agent and an API connection; this
// type configures them.
type AgentApiManifoldConfig struct {
	AgentName     string
	APICallerName string
}

// AgentApiStartFunc encapsulates the behaviour that varies among AgentApiManifolds.
type AgentApiStartFunc func(agent.Agent, base.APICaller) (worker.Worker, error)

// AgentApiManifold returns a dependency.Manifold that calls the supplied start
// func with the API and agent resources defined in the config (once those
// resources are present).
func AgentApiManifold(config AgentApiManifoldConfig, start AgentApiStartFunc) dependency.Manifold {
	return dependency.Manifold{
		Inputs: []string{
			config.AgentName,
			config.APICallerName,
		},
		Start: func(context dependency.Context) (worker.Worker, error) {
			var agent agent.Agent
			if err := context.Get(config.AgentName, &agent); err != nil {
				return nil, err
			}
			var apiCaller base.APICaller
			if err := context.Get(config.APICallerName, &apiCaller); err != nil {
				return nil, err
			}
			return start(agent, apiCaller)
		},
	}
}
