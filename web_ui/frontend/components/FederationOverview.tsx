'use client';

import LaunchIcon from '@mui/icons-material/Launch';
import { Box, Typography } from '@mui/material';
import Link from 'next/link';
import { getFederationUrls } from '@/helpers/get';
import useSWR from 'swr';

const LinkBox = ({ href, text }: { href: string; text: string }) => {
  return (
    <Link href={href}>
      <Box
        p={1}
        px={2}
        display={'flex'}
        flexDirection={'row'}
        bgcolor={'info.light'}
        borderRadius={2}
        mb={1}
      >
        <Typography sx={{ pb: 0 }}>{text}</Typography>
        <Box ml={'auto'} my={'auto'} display={'flex'}>
          <LaunchIcon />
        </Box>
      </Box>
    </Link>
  );
};

const FederationOverview = () => {
  const { data: federationUrls, error } = useSWR(
    'getFederationUrls',
    getFederationUrls,
    { fallbackData: [] }
  );

  return (
    <>
      {!Object.values(federationUrls).every((x) => x == undefined) ? (
        <Typography variant={'h4'} component={'h2'} mb={2}>
          Federation Overview
        </Typography>
      ) : null}
      {federationUrls.map(({ text, url }) => {
        if (url) {
          return <LinkBox key={text} href={url} text={text}></LinkBox>;
        }
      })}
    </>
  );
};

export default FederationOverview;
