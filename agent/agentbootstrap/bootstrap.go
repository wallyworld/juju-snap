// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package agentbootstrap

import (
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils"
	"github.com/juju/utils/series"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/cloud"
	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/controller/modelmanager"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/network"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/multiwatcher"
)

var logger = loggo.GetLogger("juju.agent.agentbootstrap")

// InitializeStateParams holds parameters used for initializing the state
// database.
type InitializeStateParams struct {
	instancecfg.StateInitializationParams

	// BootstrapMachineAddresses holds the bootstrap machine's addresses.
	BootstrapMachineAddresses []network.Address

	// BootstrapMachineJobs holds the jobs that the bootstrap machine
	// agent will run.
	BootstrapMachineJobs []multiwatcher.MachineJob

	// SharedSecret is the Mongo replica set shared secret (keyfile).
	SharedSecret string
}

// InitializeState should be called on the bootstrap machine's agent
// configuration. It uses that information to create the controller, dial the
// controller, and initialize it. It also generates a new password for the
// bootstrap machine and calls Write to save the the configuration.
//
// The cfg values will be stored in the state's ModelConfig; the
// machineCfg values will be used to configure the bootstrap Machine,
// and its constraints will be also be used for the model-level
// constraints. The connection to the controller will respect the
// given timeout parameter.
//
// InitializeState returns the newly initialized state and bootstrap
// machine. If it fails, the state may well be irredeemably compromised.
func InitializeState(
	adminUser names.UserTag,
	c agent.ConfigSetter,
	args InitializeStateParams,
	dialOpts mongo.DialOpts,
	newPolicy state.NewPolicyFunc,
) (_ *state.State, _ *state.Machine, resultErr error) {
	if c.Tag() != names.NewMachineTag(agent.BootstrapMachineId) {
		return nil, nil, errors.Errorf("InitializeState not called with bootstrap machine's configuration")
	}
	servingInfo, ok := c.StateServingInfo()
	if !ok {
		return nil, nil, errors.Errorf("state serving information not available")
	}
	// N.B. no users are set up when we're initializing the state,
	// so don't use any tag or password when opening it.
	info, ok := c.MongoInfo()
	if !ok {
		return nil, nil, errors.Errorf("stateinfo not available")
	}
	info.Tag = nil
	info.Password = c.OldPassword()

	if err := initMongoAdminUser(info.Info, dialOpts, info.Password); err != nil {
		return nil, nil, errors.Annotate(err, "failed to initialize mongo admin user")
	}

	cloudCredentials := make(map[string]cloud.Credential)
	if args.ControllerCloudCredential != nil {
		cloudCredentials[args.ControllerCloudCredentialName] = *args.ControllerCloudCredential
	}

	logger.Debugf("initializing address %v", info.Addrs)
	st, err := state.Initialize(state.InitializeParams{
		ControllerModelArgs: state.ModelArgs{
			Owner:           adminUser,
			Config:          args.ControllerModelConfig,
			Constraints:     args.ModelConstraints,
			CloudName:       args.ControllerCloudName,
			CloudRegion:     args.ControllerCloudRegion,
			CloudCredential: args.ControllerCloudCredentialName,
		},
		CloudName:                 args.ControllerCloudName,
		Cloud:                     args.ControllerCloud,
		CloudCredentials:          cloudCredentials,
		ControllerConfig:          args.ControllerConfig,
		ControllerInheritedConfig: args.ControllerInheritedConfig,
		MongoInfo:                 info,
		MongoDialOpts:             dialOpts,
		NewPolicy:                 newPolicy,
	})
	if err != nil {
		return nil, nil, errors.Errorf("failed to initialize state: %v", err)
	}
	logger.Debugf("connected to initial state")
	defer func() {
		if resultErr != nil {
			st.Close()
		}
	}()
	servingInfo.SharedSecret = args.SharedSecret
	c.SetStateServingInfo(servingInfo)

	// Filter out any LXC or LXD bridge addresses from the machine addresses.
	args.BootstrapMachineAddresses = network.FilterBridgeAddresses(args.BootstrapMachineAddresses)

	if err = initAPIHostPorts(c, st, args.BootstrapMachineAddresses, servingInfo.APIPort); err != nil {
		return nil, nil, err
	}
	ssi := paramsStateServingInfoToStateStateServingInfo(servingInfo)
	if err := st.SetStateServingInfo(ssi); err != nil {
		return nil, nil, errors.Errorf("cannot set state serving info: %v", err)
	}
	m, err := initBootstrapMachine(c, st, args)
	if err != nil {
		return nil, nil, errors.Annotate(err, "cannot initialize bootstrap machine")
	}

	// Create the initial hosted model, with the model config passed to
	// bootstrap, which contains the UUID, name for the hosted model,
	// and any user supplied config. We also copy the authorized-keys
	// from the controller model.
	attrs := make(map[string]interface{})
	for k, v := range args.HostedModelConfig {
		attrs[k] = v
	}
	attrs[config.AuthorizedKeysKey] = args.ControllerModelConfig.AuthorizedKeys()

	// TODO(axw) we shouldn't be adding credentials to model config.
	if args.ControllerCloudCredential != nil {
		for k, v := range args.ControllerCloudCredential.Attributes() {
			attrs[k] = v
		}
	}
	controllerUUID := args.ControllerConfig.ControllerUUID()
	hostedModelConfig, err := modelmanager.ModelConfigCreator{}.NewModelConfig(
		modelmanager.IsAdmin, controllerUUID, args.ControllerModelConfig, attrs,
	)
	if err != nil {
		return nil, nil, errors.Annotate(err, "creating hosted model config")
	}
	_, hostedModelState, err := st.NewModel(state.ModelArgs{
		Owner:           adminUser,
		Config:          hostedModelConfig,
		Constraints:     args.ModelConstraints,
		CloudName:       args.ControllerCloudName,
		CloudRegion:     args.ControllerCloudRegion,
		CloudCredential: args.ControllerCloudCredentialName,
	})
	if err != nil {
		return nil, nil, errors.Annotate(err, "creating hosted model")
	}
	hostedModelState.Close()

	return st, m, nil
}

func paramsStateServingInfoToStateStateServingInfo(i params.StateServingInfo) state.StateServingInfo {
	return state.StateServingInfo{
		APIPort:        i.APIPort,
		StatePort:      i.StatePort,
		Cert:           i.Cert,
		PrivateKey:     i.PrivateKey,
		CAPrivateKey:   i.CAPrivateKey,
		SharedSecret:   i.SharedSecret,
		SystemIdentity: i.SystemIdentity,
	}
}

// initMongoAdminUser adds the admin user with the specified
// password to the admin database in Mongo.
func initMongoAdminUser(info mongo.Info, dialOpts mongo.DialOpts, password string) error {
	session, err := mongo.DialWithInfo(info, dialOpts)
	if err != nil {
		return err
	}
	defer session.Close()
	return mongo.SetAdminMongoPassword(session, mongo.AdminUser, password)
}

// initBootstrapMachine initializes the initial bootstrap machine in state.
func initBootstrapMachine(c agent.ConfigSetter, st *state.State, args InitializeStateParams) (*state.Machine, error) {
	logger.Infof("initialising bootstrap machine with config: %+v", args)

	jobs := make([]state.MachineJob, len(args.BootstrapMachineJobs))
	for i, job := range args.BootstrapMachineJobs {
		machineJob, err := machineJobFromParams(job)
		if err != nil {
			return nil, errors.Errorf("invalid bootstrap machine job %q: %v", job, err)
		}
		jobs[i] = machineJob
	}
	var hardware instance.HardwareCharacteristics
	if args.BootstrapMachineHardwareCharacteristics != nil {
		hardware = *args.BootstrapMachineHardwareCharacteristics
	}
	m, err := st.AddOneMachine(state.MachineTemplate{
		Addresses:               args.BootstrapMachineAddresses,
		Series:                  series.HostSeries(),
		Nonce:                   agent.BootstrapNonce,
		Constraints:             args.BootstrapMachineConstraints,
		InstanceId:              args.BootstrapMachineInstanceId,
		HardwareCharacteristics: hardware,
		Jobs: jobs,
	})
	if err != nil {
		return nil, errors.Errorf("cannot create bootstrap machine in state: %v", err)
	}
	if m.Id() != agent.BootstrapMachineId {
		return nil, errors.Errorf("bootstrap machine expected id 0, got %q", m.Id())
	}
	// Read the machine agent's password and change it to
	// a new password (other agents will change their password
	// via the API connection).
	logger.Debugf("create new random password for machine %v", m.Id())

	newPassword, err := utils.RandomPassword()
	if err != nil {
		return nil, err
	}
	if err := m.SetPassword(newPassword); err != nil {
		return nil, err
	}
	if err := m.SetMongoPassword(newPassword); err != nil {
		return nil, err
	}
	c.SetPassword(newPassword)
	return m, nil
}

// initAPIHostPorts sets the initial API host/port addresses in state.
func initAPIHostPorts(c agent.ConfigSetter, st *state.State, addrs []network.Address, apiPort int) error {
	var hostPorts []network.HostPort
	// First try to select the correct address using the default space where all
	// API servers should be accessible on.
	spaceAddr, ok := network.SelectAddressBySpaces(addrs)
	if ok {
		logger.Debugf("selected %q as API address", spaceAddr.Value)
		hostPorts = network.AddressesWithPort([]network.Address{spaceAddr}, apiPort)
	} else {
		// Fallback to using all instead.
		hostPorts = network.AddressesWithPort(addrs, apiPort)
	}

	return st.SetAPIHostPorts([][]network.HostPort{hostPorts})
}

// machineJobFromParams returns the job corresponding to params.MachineJob.
// TODO(dfc) this function should live in apiserver/params, move there once
// state does not depend on apiserver/params
func machineJobFromParams(job multiwatcher.MachineJob) (state.MachineJob, error) {
	switch job {
	case multiwatcher.JobHostUnits:
		return state.JobHostUnits, nil
	case multiwatcher.JobManageModel:
		return state.JobManageModel, nil
	default:
		return -1, errors.Errorf("invalid machine job %q", job)
	}
}
