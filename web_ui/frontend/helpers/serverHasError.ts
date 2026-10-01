import { Edge, ServerGeneral } from '@/types';

/**
 * Health test states in which the director has no result.
 *
 * These are distinct on purpose. An origin that legitimately cannot be probed,
 * a server whose first test cycle has not finished, and a server whose
 * administrator turned tests off are all "no result" -- but none of them is a
 * failure, and painting them red is what made operators think working origins
 * were broken. Only `Error` means a test actually ran and failed.
 *
 * See https://github.com/PelicanPlatform/pelican/issues/3095.
 */
const NO_RESULT_HEALTH_STATES = [
  'Unknown',
  'Initializing',
  'Not Applicable',
  'Health Test Disabled',
];

/** The administrator explicitly turned director tests off for this server. */
const OPTED_OUT = 'Health Test Disabled';

/** Self-reported statuses that mean the server itself is unwell. */
const UNHEALTHY_SERVER_STATUSES = [
  'shutting down',
  'critical',
  'degraded',
  'warning',
];

/**
 * Whether a server of this type may decline director tests without it counting
 * against them.
 *
 * This is the asymmetry #3095 argues for, and it mirrors
 * `probeOptOutIsAcceptable` in director/health.go -- the same rule has to hold
 * in routing and in the UI, or an operator sees a green card for a server the
 * director is already penalizing.
 *
 * An origin is often run by someone other than the federation and has real
 * reasons to opt out, so it should not be punished for a supported
 * configuration. A cache joins a federation specifically to serve that
 * federation's traffic:
 *
 *   "We need to disincentivize allowing caches that join the federation to hide
 *    their status by disallowing the director to test them, because otherwise we
 *    cannot tell if it's a good cache and should rightfully deprioritize them."
 */
export const probeOptOutIsAcceptable = (server?: ServerGeneral) =>
  server?.type !== 'Cache';

/** A cache that has shut the director out. Treated as an error, per #3095. */
const cacheRefusedMeasurement = (server?: ServerGeneral) =>
  server?.healthStatus === OPTED_OUT && !probeOptOutIsAcceptable(server);

const serverHasError = (server?: ServerGeneral) => {
  const healthTestFailed = server?.healthStatus === 'Error';
  const serverReportsUnhealthy = UNHEALTHY_SERVER_STATUSES.includes(
    server?.serverStatus || ''
  );
  return (
    healthTestFailed ||
    serverReportsUnhealthy ||
    cacheRefusedMeasurement(server)
  );
};

/**
 * Whether anyone has actually measured this server.
 *
 * A server nobody is watching is not the same as one that has been verified
 * healthy, and the UI must not render them identically. This is the signal for
 * an "unverified" affordance rather than a plain green state.
 */
export const serverIsUnobserved = (server?: ServerGeneral) =>
  NO_RESULT_HEALTH_STATES.includes(server?.healthStatus || '');

/**
 * Parts of a client's path that neither the director nor the server reached.
 *
 * A server can be green on every other field and still have entries here. That
 * combination is exactly the region where a client can fail while the director
 * sees nothing wrong, so it is worth surfacing on its own rather than folding
 * into the pass/fail colour.
 */
export const uncoveredEdges = (server?: ServerGeneral): Edge[] =>
  server?.healthDetail?.uncovered || [];

/** Plain-language names for the hops a probe can miss. */
const EDGE_LABELS: Record<Edge, string> = {
  front: 'TLS and HTTP front end',
  authz: 'authorization',
  'origin-data': 'the storage clients read',
  'cache-hit': 'serving an object already cached',
  'director-resolve': 'resolving upstream through the director',
  'fed-token': 'minting a federation token',
  'upstream-fetch': 'fetching from the upstream origin',
  'cache-write': 'committing a fetched object to disk',
};

export const describeEdge = (edge: Edge): string => EDGE_LABELS[edge] || edge;

/**
 * One sentence explaining why a server has no health result, for a tooltip.
 *
 * Returns undefined when the server has been measured, so a caller can use the
 * presence of a string as the condition for showing the affordance at all.
 */
export const unobservedReason = (
  server?: ServerGeneral
): string | undefined => {
  if (!server || !serverIsUnobserved(server)) return undefined;

  switch (server.healthStatus) {
    case OPTED_OUT:
      return probeOptOutIsAcceptable(server)
        ? 'This origin declined director tests, which is a supported configuration. Its health is unverified rather than bad.'
        : 'This cache declined director tests. The federation cannot tell a good cache from a failing one, so it is deprioritized in routing.';
    case 'Not Applicable':
      return 'This server advertises no capability the director can probe, so no test applies. Nothing is wrong; nothing was checked either.';
    case 'Initializing':
      return 'The first test cycle has not finished yet.';
    default:
      return 'No director test result has arrived for this server, so its health is unverified.';
  }
};

export default serverHasError;
