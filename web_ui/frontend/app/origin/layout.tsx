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

import {Box} from "@mui/material";

import {Header} from "@/components/layout/Header";
import {Sidebar} from "@/components/layout/Sidebar";
import Link from "next/link";
import Image from "next/image";
import PelicanLogo from "@/public/static/images/PelicanPlatformLogo_Icon.png";
import IconButton from "@mui/material/IconButton";
import HomeIcon from "@mui/icons-material/Home";
import BuildIcon from "@mui/icons-material/Build";

export const metadata = {
    title: 'Pelican Origin',
    description: 'Software designed to make data distribution easy',
}

export default function RootLayout({
                                       children,
                                   }: {
    children: React.ReactNode
}) {
    return (
        <Box display={"flex"} flexDirection={"row"}>
            <Sidebar>
                <Link href={"/index.html"}>
                    <Image
                        src={PelicanLogo}
                        alt={"Pelican Logo"}
                        width={36}
                        height={36}
                    />
                </Link>
                <Box pt={3}>
                    <Link href={"/origin/index.html"}>
                        <IconButton>
                            <HomeIcon/>
                        </IconButton>
                    </Link>
                </Box>
                <Box pt={1}>
                    <Link href={"/origin/config/index.html"}>
                        <IconButton>
                            <BuildIcon/>
                        </IconButton>
                    </Link>
                </Box>
            </Sidebar>
            <Box component={"main"} p={2} pl={"90px"} display={"flex"} minHeight={"100vh"} flexGrow={1}>
                {children}
            </Box>
        </Box>
    )
}
