'use client';

import React, { useContext, useEffect, useState } from 'react';
import { Box, IconButton, Paper, Tooltip, Typography } from '@mui/material';
import { amber, grey, red } from '@mui/material/colors';
import { User } from '@/index';
import { Equalizer, HelpOutline, Language } from '@mui/icons-material';
import { NamespaceIcon } from '@/components/Namespace/index';
import Link from 'next/link';
import { alertOnError } from '@/helpers/util';
import { DirectorDropdown } from '@/app/director/components/DirectorDropdown';
import { ServerDetailed, ServerGeneral } from '@/types';
import { getDirectorServer } from '@/helpers/api';
import {
  AlertDispatchContext,
  AlertReducerAction,
} from '@/components/AlertProvider';
import serverHasError, {
  serverIsUnobserved,
  unobservedReason,
} from '@/helpers/serverHasError';

export interface DirectorCardProps {
  server: ServerGeneral;
  authenticated?: User;
}

/**
 * Three states, not two.
 *
 * A server that failed a test, a server that passed one, and a server nobody
 * measured are different things, and the last one used to render identically to
 * the second. Giving it its own colour is what stops "we did not look" from
 * reading as "we looked and it was fine".
 */
const borderColors = (server: ServerGeneral) => {
  if (serverHasError(server)) {
    return { rest: red[100], hover: red[400] };
  }
  if (serverIsUnobserved(server)) {
    return { rest: amber[200], hover: amber[400] };
  }
  return { rest: 'secondary.main', hover: grey[200] };
};

export const DirectorCard = ({ server, authenticated }: DirectorCardProps) => {
  const dispatch = useContext(AlertDispatchContext);
  const colors = borderColors(server);
  const unverified = unobservedReason(server);

  const [dropdownOpen, setDropdownOpen] = useState<boolean>(false);
  const [detailedServer, setDetailedServer] = useState<
    ServerDetailed | undefined
  >();

  // Update the detailed server when the server prop changes
  useEffect(() => {
    if (detailedServer !== undefined && dropdownOpen) {
      updateDetailedServer(server.name, setDetailedServer, dispatch);
    }
  }, [server, setDetailedServer, dispatch, dropdownOpen, detailedServer]);

  return (
    <>
      <Paper>
        <Box
          sx={{
            cursor: 'pointer',
            display: 'flex',
            width: '100%',
            justifyContent: 'space-between',
            border: 'solid #ececec 2px',
            borderRadius: '4px',
            transition: 'background-color 0.3s',
            '&:hover': {
              borderColor: colors.hover,
            },
            borderColor: colors.rest,
            p: 1,
          }}
          onClick={async () => {
            setDropdownOpen(!dropdownOpen);
          }}
        >
          <Box my={'auto'} ml={1} display={'flex'} flexDirection={'row'}>
            <NamespaceIcon
              serverType={server.type.toLowerCase() as 'cache' | 'origin'}
              bgcolor={server.filtered ? grey[500] : undefined}
            />
            <Typography sx={{ pt: '2px' }}>
              {server.name}
              {server?.version && <> &#x2022; {server.version}</>}
            </Typography>
            {unverified && (
              <Tooltip title={unverified} placement={'right'}>
                <Box
                  sx={{ display: 'flex', alignItems: 'center', ml: 1 }}
                  aria-label={'Health unverified'}
                >
                  <HelpOutline
                    fontSize={'small'}
                    sx={{ color: amber[800] }}
                    titleAccess={'Health unverified'}
                  />
                </Box>
              </Tooltip>
            )}
          </Box>
          <Box display={'flex'} flexDirection={'row'}>
            <Box my={'auto'} display={'flex'}>
              {server?.webUrl && (
                <Box ml={1}>
                  <Link href={server.webUrl} target={'_blank'}>
                    <Tooltip title={'View Server Website'}>
                      <IconButton size={'small'}>
                        <Language />
                      </IconButton>
                    </Tooltip>
                  </Link>
                </Box>
              )}
              {authenticated &&
                authenticated.role == 'admin' &&
                server?.webUrl && (
                  <Box ml={1}>
                    <Link
                      href={`/director/metrics/${server.type.toLowerCase()}/?server_name=${server.name}`}
                      target={'_blank'}
                    >
                      <Tooltip title={'View Server Metrics'}>
                        <IconButton size={'small'}>
                          <Equalizer fill={'inherit'} />
                        </IconButton>
                      </Tooltip>
                    </Link>
                  </Box>
                )}
            </Box>
          </Box>
        </Box>
      </Paper>
      <DirectorDropdown
        server={detailedServer || server}
        transition={dropdownOpen}
      />
    </>
  );
};

const updateDetailedServer = async (
  serverName: string,
  setDetailedServer: React.Dispatch<
    React.SetStateAction<ServerDetailed | undefined>
  >,
  dispatch: React.Dispatch<AlertReducerAction>
) => {
  await alertOnError(
    async () => {
      const response = await getDirectorServer(serverName);
      setDetailedServer(await response.json());
    },
    'Failed to fetch server details',
    dispatch
  );
};

export default DirectorCard;
