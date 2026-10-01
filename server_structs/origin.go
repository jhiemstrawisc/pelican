/***************************************************************
 *
 * Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package server_structs

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
)

type (
	OriginStorageType string
)

// OriginDataRoutePrefix is where a native origin mounts its exports when it
// shares a web server with a co-located director.  The director owns the root
// path space (clients resolve an object by GETting
// https://<director>/<ns>/<obj> and following the redirect), so the bytes have
// to live somewhere else.  An origin running without a director -- including a
// standalone one -- mounts its exports at their bare federation prefixes
// instead; see origin_serve.RegisterHandlers.
//
// Both ends of that arrangement must agree on this string: the origin mounts
// its handlers under it, and the ad it publishes has to point here.
const OriginDataRoutePrefix = "/api/v1.0/origin/data"

const (
	OriginStoragePosix    OriginStorageType = "posix"
	OriginStoragePosixv2  OriginStorageType = "posixv2"
	OriginStorageSSH      OriginStorageType = "ssh"
	OriginStorageS3       OriginStorageType = "s3"
	OriginStorageHTTPS    OriginStorageType = "https"
	OriginStorageGlobus   OriginStorageType = "globus"
	OriginStorageS3v2     OriginStorageType = "s3v2"     // Native S3 backend (no XRootD)
	OriginStorageHTTPSv2  OriginStorageType = "httpsv2"  // Native HTTPS/WebDAV backend (no XRootD)
	OriginStorageGlobusv2 OriginStorageType = "globusv2" // Native Globus backend (no XRootD)
	OriginStoragePStore   OriginStorageType = "pstore"   // Pelican store: encrypted block store on local disk (no XRootD)
	OriginStorageXRoot    OriginStorageType = "xroot"    // Not meant to be extensible, but facilitates legacy OSDF --> Pelican transition
)

var (
	ErrUnknownOriginStorageType = errors.New("unknown origin storage type")
)

// StorageTypeProfile describes what a backend can do, as data rather than as a
// set of scattered type switches.
//
// Every question the rest of Pelican asks about a storage type is answered from
// one row here. Before, three separate predicates -- SupportsSelfTest,
// IsPosixLike, UsesXRootD -- each carried their own switch statement and their
// own default for unlisted types, so adding a backend meant remembering all
// three and the compiler helped with none of them. Two of them silently
// disagreed: pstore reported that it supports self-tests while the launcher,
// keyed off UsesXRootD, never started one for it.
//
// Adding a backend is now one entry in storageTypeProfiles. Anything a probe
// needs beyond these declarative facts belongs in a ProbeSeeder, which is the
// behavioral escape hatch for backends that must place a probe object out of
// band.
type StorageTypeProfile struct {
	// ServedByXRootD reports whether an XRootD process fronts this backend, as
	// opposed to the pelican process serving it natively.
	ServedByXRootD bool

	// LocalFilesystem reports whether an export's StoragePrefix names a directory
	// on this host, so it can be walked, stat'd, or opened as an os.Root.
	//
	// pstore is deliberately false: its StoragePrefix is a path inside the store's
	// own namespace, not on the filesystem -- "/" is a perfectly ordinary value
	// for it -- so resolving one against the local filesystem reads something
	// else entirely, or the host's root directory.
	LocalFilesystem bool

	// CheapWrites reports whether writing a small scratch object costs about what
	// writing to a local disk costs.
	//
	// This is a cost signal, not a permission one. Whether an origin *may* write
	// is a capability question answered by its configuration; this says only
	// whether doing so every fifteen seconds is reasonable. Remote-protocol
	// backends are false because a probe there means egress and a credentialed
	// round trip on every health check, so their write probes run on a slower
	// cadence rather than not at all.
	CheapWrites bool
}

// storageTypeProfiles is the single table describing every backend.
//
// A type absent from this map gets the zero profile, which is the conservative
// reading: not local, not cheap to write, and not XRootD-fronted. That last one
// matters -- an unknown backend that silently defaulted to "XRootD fronts this"
// would have Pelican try to launch and configure a process for it.
var storageTypeProfiles = map[OriginStorageType]StorageTypeProfile{
	OriginStoragePosix:    {ServedByXRootD: true, LocalFilesystem: true, CheapWrites: true},
	OriginStoragePosixv2:  {ServedByXRootD: false, LocalFilesystem: true, CheapWrites: true},
	OriginStoragePStore:   {ServedByXRootD: false, LocalFilesystem: false, CheapWrites: true},
	OriginStorageSSH:      {ServedByXRootD: false, LocalFilesystem: false, CheapWrites: false},
	OriginStorageS3:       {ServedByXRootD: true, LocalFilesystem: false, CheapWrites: false},
	OriginStorageS3v2:     {ServedByXRootD: false, LocalFilesystem: false, CheapWrites: false},
	OriginStorageHTTPS:    {ServedByXRootD: true, LocalFilesystem: false, CheapWrites: false},
	OriginStorageHTTPSv2:  {ServedByXRootD: false, LocalFilesystem: false, CheapWrites: false},
	OriginStorageGlobus:   {ServedByXRootD: true, LocalFilesystem: false, CheapWrites: false},
	OriginStorageGlobusv2: {ServedByXRootD: false, LocalFilesystem: false, CheapWrites: false},
	OriginStorageXRoot:    {ServedByXRootD: true, LocalFilesystem: false, CheapWrites: false},
}

// Profile returns the backend's declarative facts.
func (t OriginStorageType) Profile() StorageTypeProfile {
	return storageTypeProfiles[t]
}

// IsPosixLike reports whether an export's StoragePrefix names a directory on
// this host, so it can be walked, stat'd, or opened as an os.Root.
func (t OriginStorageType) IsPosixLike() bool {
	return t.Profile().LocalFilesystem
}

// UsesXRootD reports whether an origin of this storage type is fronted by an
// XRootD process.
func (t OriginStorageType) UsesXRootD() bool {
	return t.Profile().ServedByXRootD
}

// HasCheapWrites reports whether a scratch write against this backend is cheap
// enough to perform on the ordinary probe cadence.
//
// Note what this no longer decides: whether the origin is probed at all. That
// used to be SupportsSelfTest, and a false answer disabled health testing
// outright, which is why S3-backed origins were unobservable. Cost now selects a
// cadence; capability decides what runs.
func (t OriginStorageType) HasCheapWrites() bool {
	return t.Profile().CheapWrites
}

// Convert a string to an OriginStorageType
func ParseOriginStorageType(storageType string) (ost OriginStorageType, err error) {
	switch storageType {
	case string(OriginStorageS3):
		ost = OriginStorageS3
	case string(OriginStorageHTTPS):
		ost = OriginStorageHTTPS
	case string(OriginStoragePosix):
		ost = OriginStoragePosix
	case string(OriginStoragePosixv2):
		ost = OriginStoragePosixv2
	case string(OriginStorageSSH):
		ost = OriginStorageSSH
	case string(OriginStorageXRoot):
		ost = OriginStorageXRoot
	case string(OriginStorageGlobus):
		ost = OriginStorageGlobus
	case string(OriginStorageS3v2):
		ost = OriginStorageS3v2
	case string(OriginStorageHTTPSv2):
		ost = OriginStorageHTTPSv2
	case string(OriginStorageGlobusv2):
		ost = OriginStorageGlobusv2
	case string(OriginStoragePStore):
		ost = OriginStoragePStore
	default:
		err = errors.Wrapf(ErrUnknownOriginStorageType, "storage type %s (known types are posix, posixv2, ssh, s3, s3v2, https, httpsv2, globus, globusv2, pstore, and xroot)", storageType)
	}
	return
}

// ParseExportVolume splits a docker-style export volume into the storage prefix
// it exposes and the federation prefix it is served under.  A value with no
// colon names the same path on both sides.
//
// The two sides are cleaned differently on purpose.  A storage prefix names a
// location on this host, so it takes the platform's separator; a federation
// prefix is the path half of a URL and is always slash-separated, so cleaning it
// as a filesystem path would turn "/public" into "\public" on Windows and stop
// it matching anything -- including the checks that reject a root prefix.
//
// The split is on the *first* colon, which means a storage prefix containing one
// -- a Windows drive letter, say -- cannot be expressed in this syntax.  That is
// a property of the syntax rather than a choice made here, and it is the reason
// this lives in one place: config validation and export construction disagreeing
// about where the federation prefix begins would let a prefix pass validation
// and then be served under a different name.
func ParseExportVolume(volume string) (storagePrefix, federationPrefix string) {
	storagePrefix = filepath.Clean(volume)
	federationPrefix = path.Clean(volume)
	if parts := strings.SplitN(volume, ":", 2); len(parts) == 2 {
		storagePrefix = filepath.Clean(parts[0])
		federationPrefix = path.Clean(parts[1])
	}
	return
}
