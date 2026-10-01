import { CapabilitiesChip, Dropdown, InformationSpan } from '@/components';
import { Box, Grid } from '@mui/material';
import React from 'react';
import { SinglePointMap } from '@/components/Map';
import { ServerCapabilitiesTable } from '@/components/ServerCapabilitiesTable';
import {
  Capabilities,
  ServerDetailed,
  ServerGeneral,
  TestReport,
} from '@/types';
import {
  describeEdge,
  uncoveredEdges,
  unobservedReason,
} from '@/helpers/serverHasError';

interface DirectorDropdownProps {
  server: ServerGeneral | ServerDetailed;
  transition: boolean;
}

/**
 * Names the step that produced a report's outcome, and why it did.
 *
 * The aggregate word on its own sent an operator to the server's own logs to
 * learn which of write, read, or delete broke. The director has known that all
 * along; it simply had nowhere to put it.
 */
const describeReport = (report?: TestReport): string | undefined => {
  if (!report) return undefined;

  const worst = report.results.find((r) => r.outcome === report.outcome);
  if (!worst) return report.outcome;

  const parts = [`${worst.outcome} during ${worst.op}`];
  if (worst.reason) parts.push(`(${worst.reason})`);
  if (worst.binding?.namespace) parts.push(`on ${worst.binding.namespace}`);
  if (worst.detail) parts.push(`- ${worst.detail}`);
  return parts.join(' ');
};

export const DirectorDropdown = ({
  server,
  transition,
}: DirectorDropdownProps) => {
  const detail = server.healthDetail;
  const unverified = unobservedReason(server as ServerGeneral);
  const probeSummary = describeReport(detail?.directorProbe);
  const selfTestSummary = describeReport(detail?.reported?.selfTest);
  const uncovered = uncoveredEdges(server as ServerGeneral);

  return (
    <>
      <Dropdown transition={transition} flexDirection={'column'}>
        <Grid container spacing={1}>
          <Grid
            size={{
              xs: 12,
              md: 7,
            }}
          >
            <InformationSpan name={'Type'} value={server.type} />
            <InformationSpan name={'Version'} value={server.version} />
            <InformationSpan
              name={'In Downtime'}
              value={server.filtered ? 'True' : 'False'}
            />
            <InformationSpan
              name={'Server Status'}
              value={server.serverStatus}
            />
            <InformationSpan
              name={'XRootD Health Test'}
              value={server.healthStatus}
            />
            {unverified && (
              <InformationSpan name={'Unverified Because'} value={unverified} />
            )}
            {probeSummary && (
              <InformationSpan
                name={'Director Probe'}
                value={probeSummary}
                indent={1}
              />
            )}
            {selfTestSummary && (
              <InformationSpan
                name={'Server Self Test'}
                value={selfTestSummary}
                indent={1}
              />
            )}
            {uncovered.length > 0 && (
              <InformationSpan
                name={'Not Checked By Any Test'}
                value={uncovered.map(describeEdge).join(', ')}
              />
            )}
            {detail?.detailWithheld && (
              <InformationSpan
                name={'Diagnostic Detail'}
                value={
                  'Withheld by this origin (Origin.ShareHealthDetails is false). Outcomes and coverage are still reported.'
                }
              />
            )}
            <InformationSpan name={'URL'} value={server.url} />
            <InformationSpan
              name={'Longitude'}
              value={server.longitude.toString()}
            />
            <InformationSpan
              name={'Latitude'}
              value={server.latitude.toString()}
            />
          </Grid>
          <Grid
            size={{
              xs: 12,
              md: 5,
            }}
          >
            <Box
              borderRadius={1}
              height={'100%'}
              minHeight={'140px'}
              overflow={'hidden'}
            >
              {transition && (
                <SinglePointMap
                  point={{ lat: server.latitude, lng: server.longitude }}
                />
              )}
            </Box>
          </Grid>
        </Grid>
        {['Origin', 'Namespace'].includes(server?.type) && (
          <Box sx={{ my: 1 }}>
            <ServerCapabilitiesTable server={server} />
          </Box>
        )}
      </Dropdown>
    </>
  );
};

export const CapabilitiesRow = ({
  capabilities,
  parentCapabilities,
}: {
  capabilities: Capabilities;
  parentCapabilities?: Capabilities;
}) => {
  return (
    <Grid container spacing={1}>
      {Object.entries(capabilities).map(([key, value]) => {
        const castKey = key as keyof Capabilities;
        return (
          <Grid
            key={key}
            size={{
              md: 12 / 5,
              sm: 12 / 4,
              xs: 12 / 2,
            }}
          >
            <CapabilitiesChip
              name={key}
              value={value}
              parentValue={
                parentCapabilities ? parentCapabilities[castKey] : undefined
              }
            />
          </Grid>
        );
      })}
    </Grid>
  );
};
