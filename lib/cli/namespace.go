// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/pingcap/tiproxy/lib/config"
	"github.com/spf13/cobra"
)

const (
	namespacePrefix = "/api/admin/namespace"
)

func listAllFiles(dir, ext string) ([]string, error) {
	infos, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var ret []string
	for _, info := range infos {
		fileName := info.Name()
		if filepath.Ext(fileName) == ext {
			ret = append(ret, filepath.Join(dir, fileName))
		}
	}

	return ret, nil
}

func GetNamespaceCmd(ctx *Context) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "namespace",
		Short: "",
	}

	// list all namespaces
	rootCmd.AddCommand(
		&cobra.Command{
			Use: "list",
			RunE: func(cmd *cobra.Command, _ []string) error {
				resp, err := doRequest(cmd.Context(), ctx, http.MethodGet, namespacePrefix, nil)
				if err != nil {
					return err
				}

				var nscs []config.Namespace
				if err := json.Unmarshal([]byte(resp), &nscs); err != nil {
					return err
				}
				nscsmap := make(map[string]config.Namespace, len(nscs))
				for _, nsc := range nscs {
					nscsmap[nsc.Namespace] = nsc
				}
				return toml.NewEncoder(cmd.OutOrStdout()).Encode(nscsmap)
			},
		},
	)

	// refresh namespaces
	{
		commitNamespaces := &cobra.Command{
			Use: "commit nsName...",
		}
		commitNamespaces.RunE = func(cmd *cobra.Command, args []string) error {
			resp, err := doRequest(cmd.Context(), ctx, http.MethodPost, fmt.Sprintf("%s/commit?namespaces=%s", namespacePrefix, strings.Join(args, ",")), nil)
			if err != nil {
				return err
			}

			cmd.Println(resp)
			return nil
		}
		rootCmd.AddCommand(commitNamespaces)
	}

	// get specific namespace
	{
		getNamespace := &cobra.Command{
			Use: "get nsName",
		}
		getNamespace.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmd.Help()
			}

			resp, err := doRequest(cmd.Context(), ctx, http.MethodGet, fmt.Sprintf("%s/%s", namespacePrefix, args[0]), nil)
			if err != nil {
				return err
			}

			var nsc config.Namespace
			if err := json.Unmarshal([]byte(resp), &nsc); err != nil {
				return err
			}
			nscbytes, err := nsc.ToBytes()
			if err != nil {
				return err
			}
			cmd.Println(string(nscbytes))
			return nil
		}
		rootCmd.AddCommand(getNamespace)
	}

	// put specific namespace
	{
		putNamespace := &cobra.Command{
			Use: "put",
		}
		ns := putNamespace.Flags().String("ns", "", "file, or stdin")
		putNamespace.RunE = func(cmd *cobra.Command, _ []string) error {
			in := cmd.InOrStdin()
			if *ns != "" {
				f, err := os.Open(*ns)
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			nscbytes, err := io.ReadAll(in)
			if err != nil {
				return err
			}
			nsc, err := config.NewNamespace(nscbytes)
			if err != nil {
				return err
			}
			nscbytes, err = json.Marshal(nsc)
			if err != nil {
				return err
			}

			resp, err := doRequest(cmd.Context(), ctx, http.MethodPut, fmt.Sprintf("%s/%s", namespacePrefix, nsc.Namespace), bytes.NewReader(nscbytes))
			if err != nil {
				return err
			}

			cmd.Println(resp)
			return nil
		}
		rootCmd.AddCommand(putNamespace)
	}

	// import specific namespaces
	{
		importNamespace := &cobra.Command{
			Use: "import nsdir",
		}
		importNamespace.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmd.Help()
			}

			nFiles, err := listAllFiles(args[0], ".toml")
			if err != nil {
				return err
			}

			for _, nFile := range nFiles {
				fileData, err := os.ReadFile(nFile)
				if err != nil {
					return err
				}
				nsc, err := config.NewNamespace(fileData)
				if err != nil {
					return err
				}
				nscbytes, err := json.Marshal(nsc)
				if err != nil {
					return err
				}

				resp, err := doRequest(cmd.Context(), ctx, http.MethodPut, fmt.Sprintf("%s/%s", namespacePrefix, nsc.Namespace), bytes.NewBuffer(nscbytes))
				if err != nil {
					return err
				}

				cmd.Printf("- file: %s\n%s\n", nsc.Namespace, resp)
			}

			return nil
		}
		rootCmd.AddCommand(importNamespace)
	}

	// delete specific namespace
	{
		delNamespace := &cobra.Command{
			Use: "del nsName",
		}
		delNamespace.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmd.Help()
			}

			resp, err := doRequest(cmd.Context(), ctx, http.MethodDelete, fmt.Sprintf("%s/%s", namespacePrefix, args[0]), nil)
			if err != nil {
				return err
			}

			cmd.Println(resp)
			return nil
		}
		rootCmd.AddCommand(delNamespace)
	}

	return rootCmd
}
