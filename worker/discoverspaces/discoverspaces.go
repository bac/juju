// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package discoverspaces

import (
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils/set"
	"gopkg.in/juju/names.v2"
	worker "gopkg.in/juju/worker.v1"

	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/network"
	"github.com/juju/juju/worker/catacomb"
	"github.com/juju/juju/worker/gate"
)

// Facade exposes the relevant capabilities of a *discoverspaces.API; it's
// a bit raw but at least it's easily mockable.
type Facade interface {
	CreateSpaces(params.CreateSpacesParams) (params.ErrorResults, error)
	AddSubnets(params.AddSubnetsParams) (params.ErrorResults, error)
	ListSpaces() (params.DiscoverSpacesResults, error)
	ListSubnets(params.SubnetsFilters) (params.ListSubnetsResults, error)
}

// NameFunc returns a string derived from base that is not contained in used.
type NameFunc func(base string, used set.Strings) string

// Config defines the operation of a space discovery worker.
type Config struct {

	// Facade exposes the capabilities of a controller.
	Facade Facade

	// Environ exposes the capabilities of a compute substrate.
	Environ environs.Environ

	// NewName is used to sanitise, and make unique, space names as
	// reported by an Environ (for use in juju, via the Facade). You
	// should probably set it to ConvertSpaceName.
	NewName NameFunc

	// Unlocker, if not nil, will be unlocked when the first discovery
	// attempt completes successfully.
	Unlocker gate.Unlocker
}

// Validate returns an error if the config cannot be expected to
// drive a functional worker.
func (config Config) Validate() error {
	if config.Facade == nil {
		return errors.NotValidf("nil Facade")
	}
	if config.Environ == nil {
		return errors.NotValidf("nil Environ")
	}
	if config.NewName == nil {
		return errors.NotValidf("nil NewName")
	}
	// missing Unlocker gate just means "don't bother notifying"
	return nil
}

var logger = loggo.GetLogger("juju.worker.discoverspaces")

type discoverspacesWorker struct {
	catacomb catacomb.Catacomb
	config   Config
}

// NewWorker returns a worker that will attempt to discover the
// configured Environ's spaces, and update the controller via the
// configured Facade. Names are sanitised with NewName, and any
// supplied Unlocker will be Unlock()ed when the first complete
// discovery and update succeeds.
//
// Once that update completes, the worker just waits to be Kill()ed.
// We should probably poll for changes, really, but I'm making an
// effort to preserve existing behaviour where possible.
func NewWorker(config Config) (worker.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	dw := &discoverspacesWorker{
		config: config,
	}
	err := catacomb.Invoke(catacomb.Plan{
		Site: &dw.catacomb,
		Work: dw.loop,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return dw, nil
}

// Kill is part of the worker.Worker interface.
func (dw *discoverspacesWorker) Kill() {
	dw.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (dw *discoverspacesWorker) Wait() error {
	return dw.catacomb.Wait()
}

func (dw *discoverspacesWorker) loop() (err error) {

	// TODO(mfoord): we'll have a watcher here checking if we need to
	// update the spaces/subnets definition.
	// TODO(fwereade): for now, use a changes channel that apes the
	// standard initial event behaviour, so we can make the loop
	// follow the standard structure.
	changes := make(chan struct{}, 1)
	changes <- struct{}{}

	gate := dw.config.Unlocker
	for {
		select {
		case <-dw.catacomb.Dying():
			return dw.catacomb.ErrDying()
		case <-changes:
			if err := dw.handleSubnets(); err != nil {
				return errors.Trace(err)
			}
			logger.Debugf("space discovery complete")
			if gate != nil {
				gate.Unlock()
				gate = nil
			}
		}
	}
}

func (dw *discoverspacesWorker) handleSubnets() error {
	environ, ok := environs.SupportsNetworking(dw.config.Environ)
	if !ok {
		logger.Debugf("not a networking environ")
		return nil
	}
	canDiscoverSpaces, err := environ.SupportsSpaceDiscovery()
	if err != nil {
		return errors.Trace(err)
	}
	if canDiscoverSpaces {
		return errors.Trace(dw.discoverSpaces(environ))
	}
	logger.Debugf("environ does not support space discovery")
	return errors.Trace(dw.discoverSubnetsOnly(environ))
}

func (dw *discoverspacesWorker) discoverSubnetsOnly(environ environs.NetworkingEnviron) error {
	modelSubnetIds, err := dw.getModelSubnets()
	if err != nil {
		return errors.Trace(err)
	}
	subnets, err := environ.Subnets(instance.UnknownId, nil)
	if err != nil {
		return errors.Trace(err)
	}
	var addSubnetsArgs params.AddSubnetsParams
	collectMissingSubnets(&addSubnetsArgs, "", modelSubnetIds, subnets)
	logger.Debugf("Adding missing subnets: %#v", addSubnetsArgs)
	return errors.Trace(dw.addSubnetsFromArgs(addSubnetsArgs))
}

func (dw *discoverspacesWorker) discoverSpaces(environ environs.NetworkingEnviron) error {
	providerSpaces, err := environ.Spaces()
	if err != nil {
		return errors.Trace(err)
	}

	listSpacesResult, err := dw.config.Facade.ListSpaces()
	if err != nil {
		return errors.Trace(err)
	}
	modelSpaceMap := make(map[string]params.ProviderSpace)
	spaceNames := make(set.Strings)
	for _, space := range listSpacesResult.Results {
		modelSpaceMap[space.ProviderId] = space
		spaceNames.Add(space.Name)
	}

	modelSubnetIds, err := dw.getModelSubnets()
	if err != nil {
		return errors.Trace(err)
	}

	// TODO(mfoord): we need to delete spaces and subnets that no longer
	// exist, so long as they're not in use.
	var createSpacesArgs params.CreateSpacesParams
	var addSubnetsArgs params.AddSubnetsParams
	for _, space := range providerSpaces {
		// Check if the space is already in state, in which case we know
		// its name.
		stateSpace, ok := modelSpaceMap[string(space.ProviderId)]
		var spaceTag names.SpaceTag
		if ok {
			spaceName := stateSpace.Name
			if !names.IsValidSpace(spaceName) {
				// Can only happen if an invalid name is stored
				// in state.
				logger.Errorf("space %q has an invalid name, ignoring", spaceName)
				continue

			}
			spaceTag = names.NewSpaceTag(spaceName)

		} else {
			// The space is new, we need to create a valid name for it
			// in state.
			spaceName := string(space.Name)
			// Convert the name into a valid name that isn't already in
			// use.
			spaceName = dw.config.NewName(spaceName, spaceNames)
			spaceNames.Add(spaceName)
			spaceTag = names.NewSpaceTag(spaceName)
			// We need to create the space.
			createSpacesArgs.Spaces = append(createSpacesArgs.Spaces, params.CreateSpaceParams{
				Public:     false,
				SpaceTag:   spaceTag.String(),
				ProviderId: string(space.ProviderId),
			})
		}

		collectMissingSubnets(&addSubnetsArgs, spaceTag.String(), modelSubnetIds, space.Subnets)
	}

	if err := dw.createSpacesFromArgs(createSpacesArgs); err != nil {
		return errors.Trace(err)
	}

	if err := dw.addSubnetsFromArgs(addSubnetsArgs); err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (dw *discoverspacesWorker) createSpacesFromArgs(createSpacesArgs params.CreateSpacesParams) error {
	facade := dw.config.Facade

	expectedNumCreated := len(createSpacesArgs.Spaces)
	if expectedNumCreated > 0 {
		result, err := facade.CreateSpaces(createSpacesArgs)
		if err != nil {
			return errors.Annotate(err, "creating spaces failed")
		}
		if len(result.Results) != expectedNumCreated {
			return errors.Errorf(
				"unexpected response from CreateSpaces: expected %d results, got %d",
				expectedNumCreated, len(result.Results),
			)
		}
		for _, res := range result.Results {
			if res.Error != nil {
				return errors.Annotate(res.Error, "creating space failed")
			}
		}
		logger.Debugf("discovered and imported %d spaces: %v", expectedNumCreated, createSpacesArgs)
	} else {
		logger.Debugf("no unknown spaces discovered for import")
	}

	return nil
}

func (dw *discoverspacesWorker) addSubnetsFromArgs(addSubnetsArgs params.AddSubnetsParams) error {
	facade := dw.config.Facade

	expectedNumAdded := len(addSubnetsArgs.Subnets)
	if expectedNumAdded > 0 {
		result, err := facade.AddSubnets(addSubnetsArgs)
		if err != nil {
			return errors.Annotate(err, "adding subnets failed")
		}
		if len(result.Results) != expectedNumAdded {
			return errors.Errorf(
				"unexpected response from AddSubnets: expected %d results, got %d",
				expectedNumAdded, len(result.Results),
			)
		}
		for _, res := range result.Results {
			if res.Error != nil {
				return errors.Annotate(res.Error, "adding subnet failed")
			}
		}
		logger.Debugf("discovered and imported %d subnets: %v", expectedNumAdded, addSubnetsArgs)
	} else {
		logger.Debugf("no unknown subnets discovered for import")
	}

	return nil
}

func (dw *discoverspacesWorker) getModelSubnets() (set.Strings, error) {
	modelSubnets, err := dw.config.Facade.ListSubnets(params.SubnetsFilters{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	modelSubnetIds := make(set.Strings)
	for _, subnet := range modelSubnets.Results {
		modelSubnetIds.Add(subnet.ProviderId)
	}
	return modelSubnetIds, nil
}

func collectMissingSubnets(
	addArgs *params.AddSubnetsParams,
	spaceTag string,
	existingSubnets set.Strings,
	subnets []network.SubnetInfo,
) {
	// TODO(mfoord): currently no way of removing subnets, or
	// changing the space they're in, so we can only add ones we
	// don't already know about.
	for _, subnet := range subnets {
		if existingSubnets.Contains(string(subnet.ProviderId)) {
			continue
		}
		zones := subnet.AvailabilityZones
		if len(zones) == 0 {
			logger.Tracef(
				"provider does not specify zones for subnet %q; using 'default' zone as fallback",
				subnet.CIDR,
			)
			zones = []string{"default"}
		}
		addArgs.Subnets = append(addArgs.Subnets, params.AddSubnetParams{
			SubnetProviderId: string(subnet.ProviderId),
			SubnetTag:        names.NewSubnetTag(subnet.CIDR).String(),
			SpaceTag:         spaceTag,
			VLANTag:          subnet.VLANTag,
			Zones:            zones,
		})
	}
}
