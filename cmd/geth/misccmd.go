// Copyright 2016 The go-AVNereum Authors
// This file is part of go-AVNereum.
//
// go-AVNereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-AVNereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-AVNereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/AVNereum/go-AVNereum/cmd/utils"
	"github.com/AVNereum/go-AVNereum/consensus/AVNash"
	"github.com/AVNereum/go-AVNereum/params"
	"gopkg.in/urfave/cli.v1"
)

var (
	VersionCheckUrlFlag = cli.StringFlag{
		Name:  "check.url",
		Usage: "URL to use when checking vulnerabilities",
		Value: "https://gAVN.AVNereum.org/docs/vulnerabilities/vulnerabilities.json",
	}
	VersionCheckVersionFlag = cli.StringFlag{
		Name:  "check.version",
		Usage: "Version to check",
		Value: fmt.Sprintf("GAVN/v%v/%v-%v/%v",
			params.VersionWithCommit(gitCommit, gitDate),
			runtime.GOOS, runtime.GOARCH, runtime.Version()),
	}
	makecacheCommand = cli.Command{
		Action:    utils.MigrateFlags(makecache),
		Name:      "makecache",
		Usage:     "Generate AVNash verification cache (for testing)",
		ArgsUsage: "<blockNum> <outputDir>",
		Category:  "MISCELLANEOUS COMMANDS",
		Description: `
The makecache command generates an AVNash cache in <outputDir>.

This command exists to support the system testing project.
Regular users do not need to execute it.
`,
	}
	makedagCommand = cli.Command{
		Action:    utils.MigrateFlags(makedag),
		Name:      "makedag",
		Usage:     "Generate AVNash mining DAG (for testing)",
		ArgsUsage: "<blockNum> <outputDir>",
		Category:  "MISCELLANEOUS COMMANDS",
		Description: `
The makedag command generates an AVNash DAG in <outputDir>.

This command exists to support the system testing project.
Regular users do not need to execute it.
`,
	}
	versionCommand = cli.Command{
		Action:    utils.MigrateFlags(version),
		Name:      "version",
		Usage:     "Print version numbers",
		ArgsUsage: " ",
		Category:  "MISCELLANEOUS COMMANDS",
		Description: `
The output of this command is supposed to be machine-readable.
`,
	}
	versionCheckCommand = cli.Command{
		Action: utils.MigrateFlags(versionCheck),
		Flags: []cli.Flag{
			VersionCheckUrlFlag,
			VersionCheckVersionFlag,
		},
		Name:      "version-check",
		Usage:     "Checks (online) whAVNer the current version suffers from any known security vulnerabilities",
		ArgsUsage: "<versionstring (optional)>",
		Category:  "MISCELLANEOUS COMMANDS",
		Description: `
The version-check command fetches vulnerability-information from https://gAVN.AVNereum.org/docs/vulnerabilities/vulnerabilities.json, 
and displays information about any security vulnerabilities that affect the currently executing version.
`,
	}
	licenseCommand = cli.Command{
		Action:    utils.MigrateFlags(license),
		Name:      "license",
		Usage:     "Display license information",
		ArgsUsage: " ",
		Category:  "MISCELLANEOUS COMMANDS",
	}
)

// makecache generates an AVNash verification cache into the provided folder.
func makecache(ctx *cli.Context) error {
	args := ctx.Args()
	if len(args) != 2 {
		utils.Fatalf(`Usage: gAVN makecache <block number> <outputdir>`)
	}
	block, err := strconv.ParseUint(args[0], 0, 64)
	if err != nil {
		utils.Fatalf("Invalid block number: %v", err)
	}
	AVNash.MakeCache(block, args[1])

	return nil
}

// makedag generates an AVNash mining DAG into the provided folder.
func makedag(ctx *cli.Context) error {
	args := ctx.Args()
	if len(args) != 2 {
		utils.Fatalf(`Usage: gAVN makedag <block number> <outputdir>`)
	}
	block, err := strconv.ParseUint(args[0], 0, 64)
	if err != nil {
		utils.Fatalf("Invalid block number: %v", err)
	}
	AVNash.MakeDataset(block, args[1])

	return nil
}

func version(ctx *cli.Context) error {
	fmt.Println(strings.Title(clientIdentifier))
	fmt.Println("Version:", params.VersionWithMeta)
	if gitCommit != "" {
		fmt.Println("Git Commit:", gitCommit)
	}
	if gitDate != "" {
		fmt.Println("Git Commit Date:", gitDate)
	}
	fmt.Println("Architecture:", runtime.GOARCH)
	fmt.Println("Go Version:", runtime.Version())
	fmt.Println("Operating System:", runtime.GOOS)
	fmt.Printf("GOPATH=%s\n", os.Getenv("GOPATH"))
	fmt.Printf("GOROOT=%s\n", runtime.GOROOT())
	return nil
}

func license(_ *cli.Context) error {
	fmt.Println(`GAVN is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

GAVN is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with gAVN. If not, see <http://www.gnu.org/licenses/>.`)
	return nil
}
