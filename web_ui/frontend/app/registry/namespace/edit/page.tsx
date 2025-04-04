/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
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

'use client';

import { PutPage } from '@/app/registry/components/PutPage';
import { putGeneralNamespace } from '@/helpers/api';
import { Box, Grid, Typography } from '@mui/material';
import React from 'react';
import AuthenticatedContent from '@/components/layout/AuthenticatedContent';

export default function Page() {
  const putCache = async (data: any) => {
    return putGeneralNamespace(data);
  };

  return (
    <Box width={'100%'}>
      <Grid container>
        <Grid item xs={12}>
          <Typography variant={'h4'} pb={3}>
            Namespace Registry
          </Typography>
          <Typography variant={'h5'} pb={3}>
            Edit Namespace
          </Typography>
        </Grid>
        <Grid item xs={12}>
          <AuthenticatedContent redirect={true}>
            <PutPage update={putCache} />
          </AuthenticatedContent>
        </Grid>
      </Grid>
    </Box>
  );
}
