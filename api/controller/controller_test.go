// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package controller_test

import (
	"fmt"
	"time"

	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/api/base"
	"github.com/juju/juju/api/controller"
	commontesting "github.com/juju/juju/apiserver/common/testing"
	"github.com/juju/juju/apiserver/params"
	jujutesting "github.com/juju/juju/juju/testing"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/multiwatcher"
	"github.com/juju/juju/testing"
	"github.com/juju/juju/testing/factory"
)

type controllerSuite struct {
	jujutesting.JujuConnSuite
	commontesting.BlockHelper
}

var _ = gc.Suite(&controllerSuite{})

func (s *controllerSuite) SetUpTest(c *gc.C) {
	s.JujuConnSuite.SetUpTest(c)
}

func (s *controllerSuite) OpenAPI(c *gc.C) *controller.Client {
	return controller.NewClient(s.APIState)
}

func (s *controllerSuite) TestAllModels(c *gc.C) {
	owner := names.NewUserTag("user@remote")
	s.Factory.MakeModel(c, &factory.ModelParams{
		Name: "first", Owner: owner}).Close()
	s.Factory.MakeModel(c, &factory.ModelParams{
		Name: "second", Owner: owner}).Close()

	sysManager := s.OpenAPI(c)
	envs, err := sysManager.AllModels()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(envs, gc.HasLen, 3)

	var obtained []string
	for _, env := range envs {
		obtained = append(obtained, fmt.Sprintf("%s/%s", env.Owner, env.Name))
	}
	expected := []string{
		"admin@local/controller",
		"user@remote/first",
		"user@remote/second",
	}
	c.Assert(obtained, jc.SameContents, expected)
}

func (s *controllerSuite) TestModelConfig(c *gc.C) {
	sysManager := s.OpenAPI(c)
	cfg, err := sysManager.ModelConfig()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(cfg["name"], gc.Equals, "controller")
}

func (s *controllerSuite) TestControllerConfig(c *gc.C) {
	sysManager := s.OpenAPI(c)
	cfg, err := sysManager.ControllerConfig()
	c.Assert(err, jc.ErrorIsNil)
	cfgFromDB, err := s.State.ControllerConfig()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(cfg["controller-uuid"], gc.Equals, cfgFromDB.ControllerUUID())
	c.Assert(int(cfg["state-port"].(float64)), gc.Equals, cfgFromDB.StatePort())
	c.Assert(int(cfg["api-port"].(float64)), gc.Equals, cfgFromDB.APIPort())
}

func (s *controllerSuite) TestDestroyController(c *gc.C) {
	st := s.Factory.MakeModel(c, &factory.ModelParams{Name: "foo"})
	factory.NewFactory(st).MakeMachine(c, nil) // make it non-empty
	st.Close()

	sysManager := s.OpenAPI(c)
	err := sysManager.DestroyController(false)
	c.Assert(err, gc.ErrorMatches, `failed to destroy model: hosting 1 other models \(controller has hosted models\)`)
}

func (s *controllerSuite) TestListBlockedModels(c *gc.C) {
	err := s.State.SwitchBlockOn(state.ChangeBlock, "change block for controller")
	err = s.State.SwitchBlockOn(state.DestroyBlock, "destroy block for controller")
	c.Assert(err, jc.ErrorIsNil)

	sysManager := s.OpenAPI(c)
	results, err := sysManager.ListBlockedModels()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, jc.DeepEquals, []params.ModelBlockInfo{
		{
			Name:     "controller",
			UUID:     s.State.ModelUUID(),
			OwnerTag: s.AdminUserTag(c).String(),
			Blocks: []string{
				"BlockChange",
				"BlockDestroy",
			},
		},
	})
}

func (s *controllerSuite) TestRemoveBlocks(c *gc.C) {
	s.State.SwitchBlockOn(state.DestroyBlock, "TestBlockDestroyModel")
	s.State.SwitchBlockOn(state.ChangeBlock, "TestChangeBlock")

	sysManager := s.OpenAPI(c)
	err := sysManager.RemoveBlocks()
	c.Assert(err, jc.ErrorIsNil)

	blocks, err := s.State.AllBlocksForController()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(blocks, gc.HasLen, 0)
}

func (s *controllerSuite) TestWatchAllModels(c *gc.C) {
	// The WatchAllModels infrastructure is comprehensively tested
	// else. This test just ensure that the API calls work end-to-end.
	sysManager := s.OpenAPI(c)

	w, err := sysManager.WatchAllModels()
	c.Assert(err, jc.ErrorIsNil)
	defer func() {
		err := w.Stop()
		c.Assert(err, jc.ErrorIsNil)
	}()

	deltasC := make(chan []multiwatcher.Delta)
	go func() {
		deltas, err := w.Next()
		c.Assert(err, jc.ErrorIsNil)
		deltasC <- deltas
	}()

	select {
	case deltas := <-deltasC:
		c.Assert(deltas, gc.HasLen, 1)
		modelInfo := deltas[0].Entity.(*multiwatcher.ModelInfo)

		env, err := s.State.Model()
		c.Assert(err, jc.ErrorIsNil)

		c.Assert(modelInfo.ModelUUID, gc.Equals, env.UUID())
		c.Assert(modelInfo.Name, gc.Equals, env.Name())
		c.Assert(modelInfo.Life, gc.Equals, multiwatcher.Life("alive"))
		c.Assert(modelInfo.Owner, gc.Equals, env.Owner().Id())
		c.Assert(modelInfo.ControllerUUID, gc.Equals, env.ControllerUUID())
	case <-time.After(testing.LongWait):
		c.Fatal("timed out")
	}
}

func (s *controllerSuite) TestModelStatus(c *gc.C) {
	controller := s.OpenAPI(c)
	modelTag := s.State.ModelTag()
	results, err := controller.ModelStatus(modelTag)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, jc.DeepEquals, []base.ModelStatus{{
		UUID:               modelTag.Id(),
		HostedMachineCount: 0,
		ServiceCount:       0,
		Owner:              "admin@local",
		Life:               params.Alive,
	}})
}

func (s *controllerSuite) TestInitiateModelMigration(c *gc.C) {
	st := s.Factory.MakeModel(c, nil)
	defer st.Close()

	_, err := st.LatestModelMigration()
	c.Assert(errors.IsNotFound(err), jc.IsTrue)

	spec := controller.ModelMigrationSpec{
		ModelUUID:            st.ModelUUID(),
		TargetControllerUUID: randomUUID(),
		TargetAddrs:          []string{"1.2.3.4:5"},
		TargetCACert:         "cert",
		TargetUser:           "someone",
		TargetPassword:       "secret",
	}

	controller := s.OpenAPI(c)
	id, err := controller.InitiateModelMigration(spec)
	c.Assert(err, jc.ErrorIsNil)
	expectedId := st.ModelUUID() + ":0"
	c.Check(id, gc.Equals, expectedId)

	// Check database.
	mig, err := st.LatestModelMigration()
	c.Assert(err, jc.ErrorIsNil)
	c.Check(mig.Id(), gc.Equals, expectedId)
}

func (s *controllerSuite) TestInitiateModelMigrationError(c *gc.C) {
	spec := controller.ModelMigrationSpec{
		ModelUUID:            randomUUID(), // Model doesn't exist.
		TargetControllerUUID: randomUUID(),
		TargetAddrs:          []string{"1.2.3.4:5"},
		TargetCACert:         "cert",
		TargetUser:           "someone",
		TargetPassword:       "secret",
	}

	controller := s.OpenAPI(c)
	id, err := controller.InitiateModelMigration(spec)
	c.Check(id, gc.Equals, "")
	c.Check(err, gc.ErrorMatches, "unable to read model: .+")
}

func randomUUID() string {
	return utils.MustNewUUID().String()
}
