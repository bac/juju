// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package remotestate

import (
	"github.com/juju/names"
	"gopkg.in/juju/charm.v5"

	"github.com/juju/juju/apiserver/params"
)

// Snapshot is a snapshot of the remote state of the unit.
type Snapshot struct {
	// Life is the lifecycle state of the unit.
	Life params.Life

	// Relations contains the lifecycle states of
	// each of the service's relations, keyed by
	// relation IDs.
	Relations map[int]RelationSnapshot

	// Storage contains the lifecycle and attached
	// states of each of the unit's storage attachments.
	Storage map[names.StorageTag]StorageSnapshot

	// CharmURL is the charm URL that the unit is
	// expected to run.
	CharmURL *charm.URL

	// ForceCharmUpgrade reports whether the unit
	// should upgrade even in an error state.
	ForceCharmUpgrade bool

	// ResolvedMode reports the method of resolving
	// hook execution errors.
	ResolvedMode params.ResolvedMode

	// ConfigVersion is the last published version of
	// the unit's config settings.
	ConfigVersion int

	// Leader indicates whether or not the unit is the
	// elected leader.
	Leader bool

	// LeaderSettingsVersion is the last published
	// version of the leader settings for the service.
	LeaderSettingsVersion int

	// TODO (mattyw) Are these pending actions?
	Actions []string
}

type RelationSnapshot struct {
	Life    params.Life
	Members map[string]int64
}

// StorageSnapshot has information relating to a storage
// instance belonging to a unit.
type StorageSnapshot struct {
	Kind     params.StorageKind
	Life     params.Life
	Attached bool
	Location string
}
