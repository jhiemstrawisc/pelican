/***************************************************************
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
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

// Two levels of capability, and why they are not the same thing.
//
// Pelican has carried one Capabilities struct used at two levels without saying
// what distinguishes them. Origin.EnableWrites and Exports[].Capabilities.Writes
// look interchangeable, the top-level values are copied down as defaults for
// exports that do not set their own, and the difference is lost.
//
// They answer different questions:
//
//   - Origin level -- STORE ABILITY. Can this process perform this operation
//     against its backing store at all? A property of the backend, the
//     credentials, and the mount: "these S3 keys can write to this bucket",
//     "this filesystem is mounted read-write". Not a policy statement.
//
//   - Export level -- NAMESPACE POLICY. Is this origin willing to perform this
//     operation on behalf of this federation prefix? A property of what the
//     administrator has decided to offer: "/ligo/frames is served read-only even
//     though the disk under it is writable".
//
// From that follows the invariant this file enforces: namespace policy cannot
// exceed store ability. An export may narrow what the store can do; it may not
// widen it. Offering writes on a namespace whose store cannot be written is not
// a stricter policy, it is a promise that fails on first use.
//
// The distinction earns its keep in health testing, where the two levels are
// probed by different things: the store-ability question is answered by writing
// a scratch object into an export's real storage, and the namespace-policy
// question by exercising the federation path a client would use. Conflating
// them is how an origin came to advertise capabilities nothing had verified.
package server_structs

import (
	"fmt"
	"strings"
)

// CapabilityLevel names which of the two questions a Capabilities value answers.
type CapabilityLevel string

const (
	// StoreCapability describes what the origin process can do to its backing
	// store, given its backend and credentials.
	StoreCapability CapabilityLevel = "store"
	// NamespaceCapability describes what the origin is willing to do on behalf
	// of a particular federation prefix.
	NamespaceCapability CapabilityLevel = "namespace"
)

// Subset reports whether every operation c permits is also permitted by outer.
//
// Used to check namespace policy against store ability. Note that PublicReads is
// deliberately excluded: whether a namespace requires a token is an
// authorization decision that has nothing to do with what the store can do, and
// a store has no notion of "public".
func (c Capabilities) Subset(outer Capabilities) bool {
	return len(c.exceeding(outer)) == 0
}

// exceeding returns the names of the operations c permits that outer does not.
func (c Capabilities) exceeding(outer Capabilities) []string {
	var over []string
	if c.Reads && !outer.Reads {
		over = append(over, "Reads")
	}
	if c.Writes && !outer.Writes {
		over = append(over, "Writes")
	}
	if c.Listings && !outer.Listings {
		over = append(over, "Listings")
	}
	if c.DirectReads && !outer.DirectReads {
		over = append(over, "DirectReads")
	}
	return over
}

// ValidateNamespaceCapabilities reports whether a namespace's declared policy
// stays within what the store can do.
//
// The error names every operation that exceeds, rather than the first, so an
// administrator fixing a misconfiguration sees the whole problem at once.
func ValidateNamespaceCapabilities(prefix string, ns, store Capabilities) error {
	over := ns.exceeding(store)
	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf(
		"namespace %s declares %s, but the origin's storage does not support %s; "+
			"a namespace may offer less than its backing store can do, never more",
		prefix, strings.Join(over, " and "), strings.Join(over, " and "))
}

// Intersect returns the operations permitted at both levels: the more
// restrictive of store ability and namespace policy.
//
// WHERE THIS IS APPLIED, and why not everywhere:
//
// It is applied at the point of use -- probe planning -- and paired with a
// startup warning from ValidateOriginCapabilities, rather than by rewriting each
// export's stored capabilities during configuration.
//
// Clamping the stored config was tried and reverted. Store ability is read from
// the Origin.Enable* parameters, so it is only meaningful once server defaults
// have been applied; anything that assembles exports earlier sees every
// capability as false, and a destructive clamp there silently strips an origin of
// everything it offers. An ordering-dependent config rewrite that can disable a
// whole origin is a worse failure than the misconfiguration it was meant to
// correct, and it fails silently, which is worse still.
//
// So: warn loudly where the disagreement is visible to an administrator, and
// take the more restrictive reading where acting on the wrong value would
// actually cause harm -- a probe that is guaranteed to fail and would report a
// healthy origin as broken.
//
// This is what an origin should actually advertise for a namespace: the
// administrator's intent, bounded by what the store can deliver. Using it means
// a misconfigured export degrades to something honest rather than advertising a
// promise that fails on first use.
func (c Capabilities) Intersect(outer Capabilities) Capabilities {
	return Capabilities{
		// PublicReads is deliberately bounded by the store's *read* ability rather
		// than by its PublicReads. Whether a namespace requires a token is an
		// authorization decision with no counterpart at the store -- a filesystem
		// has no notion of "public" -- so the top-level value is a default for
		// exports that do not state their own, not a ceiling on those that do.
		// Treating it as a ceiling would silently strip public access from the
		// very common configuration of an origin-wide default of false with
		// per-export PublicReads: true, since Origin.EnablePublicReads defaults to
		// false. What the store genuinely constrains is that public *reads* are
		// still reads, so they cannot outlive an unreadable store.
		PublicReads: c.PublicReads && outer.Reads,

		// These are store abilities: whether the process, with its credentials and
		// its mount, can perform the operation at all. A namespace may offer less
		// than the store can do and never more.
		Reads:    c.Reads && outer.Reads,
		Writes:   c.Writes && outer.Writes,
		Listings: c.Listings && outer.Listings,

		// DirectReads is a routing policy rather than a store ability, but it is
		// bounded anyway: an administrator who turns it off origin-wide means it,
		// and the more restrictive of the two is the right reading. This is safe
		// where PublicReads is not because Origin.EnableDirectReads defaults to
		// true, so the clamp only bites when someone has explicitly disabled it.
		DirectReads: c.DirectReads && outer.DirectReads,

		// A copy is a write.
		Copies: c.Copies && outer.Writes && outer.Copies,
	}
}

// AnyReads reports whether the value permits reading by any route.
//
// PublicReads implies Reads throughout Pelican, but the two are set
// independently in configuration, so asking for either directly is a recurring
// source of missed cases.
func (c Capabilities) AnyReads() bool {
	return c.Reads || c.PublicReads
}
