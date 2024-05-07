package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/imdario/mergo"
	"github.com/pkg/errors"
	"github.com/score-spec/score-go/framework"
	scoreloader "github.com/score-spec/score-go/loader"
	scoreschema "github.com/score-spec/score-go/schema"
	scoretypes "github.com/score-spec/score-go/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/score-spec/score-k8s/internal"
	"github.com/score-spec/score-k8s/internal/convert"
	"github.com/score-spec/score-k8s/internal/project"
	"github.com/score-spec/score-k8s/internal/provisioners"
	"github.com/score-spec/score-k8s/internal/provisioners/loader"
)

var rootCmd = &cobra.Command{
	Use:           "score-k8s",
	SilenceErrors: true,
}

const projectDirectory = ".score-k8s"
const stateFileName = "state.yaml"
const manifestsDirectory = "manifests"

const (
	generateCmdOverridesFileFlag    = "overrides-file"
	generateCmdOverridePropertyFlag = "override-property"
	generateCmdImageFlag            = "image"
	generateCmdOutputFlag           = "output"
)

var initCmd = &cobra.Command{
	Use:           "init",
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		if err := os.MkdirAll(projectDirectory, 0700); err != nil {
			return errors.Wrap(err, "failed to ensure state directory")
		}

		stateFile := filepath.Join(projectDirectory, stateFileName)
		if _, err := os.Stat(stateFile); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return errors.Wrapf(err, "failed to check for existing state file")
			}
			if f, err := os.OpenFile(stateFile, os.O_CREATE|os.O_WRONLY, 0600); err != nil {
				if errors.Is(err, os.ErrExist) {
					return errors.Errorf("state file '%s' already exists", stateFile)
				}
				return errors.Wrap(err, "failed to open empty project state")
			} else {
				defer f.Close()
				state := new(framework.State[framework.NoExtras, framework.NoExtras])
				if err := yaml.NewEncoder(f).Encode(state); err != nil {
					return errors.Wrap(err, "failed to write empty project state")
				}
				slog.Info("Created empty project state", "file", stateFile)
			}
		} else {
			slog.Info("Skipping creation of state file since it already exists")
		}

		defaultProvisioners := filepath.Join(projectDirectory, "zz-default.provisioners.yaml")
		if _, err := os.Stat(defaultProvisioners); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return errors.Wrapf(err, "failed to check for existing default provisioners file")
			}
			if f, err := os.OpenFile(defaultProvisioners, os.O_CREATE|os.O_WRONLY, 0600); err != nil {
				if errors.Is(err, os.ErrExist) {
					return errors.Errorf("default provisioners file '%s' already exists", defaultProvisioners)
				}
				return errors.Wrap(err, "failed to open empty provisioners file")
			} else {
				defer f.Close()
				if err := yaml.NewEncoder(f).Encode(provisioners.DefaultProvisioners); err != nil {
					return errors.Wrap(err, "failed to write empty project state")
				}
				slog.Info("Created default provisioners file", "file", defaultProvisioners)
			}
		} else {
			slog.Info("Skipping creation of default provisioners file since it already exists")
		}

		if _, err := os.Stat("score.yaml"); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return errors.Wrapf(err, "failed to check for existing score.yaml")
			}
			workload := &scoretypes.Workload{
				ApiVersion: "score.dev/v1b1",
				Metadata: map[string]interface{}{
					"name": "example",
				},
				Containers: map[string]scoretypes.Container{
					"main": {
						Image: "stefanprodan/podinfo",
					},
				},
			}
			if f, err := os.OpenFile("score.yaml", os.O_CREATE|os.O_WRONLY, 0600); err != nil {
				return errors.Wrap(err, "failed to open empty score.yaml file")
			} else {
				defer f.Close()
				if err := yaml.NewEncoder(f).Encode(workload); err != nil {
					return errors.Wrap(err, "failed to write empty score.yaml file")
				}
				slog.Info("Created empty score.yaml file", "file", "score.yaml")
			}
		} else {
			slog.Info("Skipping creation of score.yaml file since it already exists")
		}

		return nil
	},
}

var generateCmd = &cobra.Command{
	Use:           "generate",
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		stateFile := filepath.Join(projectDirectory, stateFileName)
		var state *project.State

		if raw, err := os.ReadFile(stateFile); err != nil {
			return errors.Wrap(err, "failed to read existing state file")
		} else {
			enc := yaml.NewDecoder(bytes.NewReader(raw))
			enc.KnownFields(false)
			var rawState project.State
			if err = enc.Decode(&rawState); err != nil {
				return errors.Wrap(err, "failed to load existing state")
			}
			state = &rawState
			slog.Info("Loaded project state", "file", stateFile, "#workloads", len(state.Workloads), "#resources", len(state.Resources))
		}

		if len(args) != 1 && (cmd.Flags().Lookup(generateCmdOverridesFileFlag).Changed || cmd.Flags().Lookup(generateCmdOverridePropertyFlag).Changed) {
			return errors.Errorf("cannot use --%s or --%s when 0 or more than 1 score files are provided", generateCmdOverridePropertyFlag, generateCmdOverridesFileFlag)
		}

		slices.Sort(args)
		for _, arg := range args {
			var rawWorkload map[string]interface{}
			if raw, err := os.ReadFile(arg); err != nil {
				return errors.Wrapf(err, "failed to read input score file: %s", arg)
			} else if err = yaml.Unmarshal(raw, &rawWorkload); err != nil {
				return errors.Wrapf(err, "failed to decode input score file: %s", arg)
			}

			// apply overrides

			if v, _ := cmd.Flags().GetString(generateCmdOverridesFileFlag); v != "" {
				if err := parseAndApplyOverrideFile(v, generateCmdOverridesFileFlag, rawWorkload); err != nil {
					return err
				}
			}

			// Now read, parse, and apply any override properties to the score files
			var err error
			if v, _ := cmd.Flags().GetStringArray(generateCmdOverridePropertyFlag); len(v) > 0 {
				for _, overridePropertyEntry := range v {
					if rawWorkload, err = parseAndApplyOverrideProperty(overridePropertyEntry, generateCmdOverridePropertyFlag, rawWorkload); err != nil {
						return err
					}
				}
			}

			// Ensure transforms are applied (be a good citizen)
			if changes, err := scoreschema.ApplyCommonUpgradeTransforms(rawWorkload); err != nil {
				return fmt.Errorf("failed to upgrade spec: %w", err)
			} else if len(changes) > 0 {
				for _, change := range changes {
					slog.Info(fmt.Sprintf("Applying backwards compatible upgrade %s", change))
				}
			}

			var workload scoretypes.Workload
			if err = scoreschema.Validate(rawWorkload); err != nil {
				return errors.Wrapf(err, "invalid score file: %s", arg)
			} else if err = scoreloader.MapSpec(&workload, rawWorkload); err != nil {
				return errors.Wrapf(err, "failed to decode input score file: %s", arg)
			}

			// Apply image override
			for containerName, container := range workload.Containers {
				if container.Image == "." {
					if v, _ := cmd.Flags().GetString(generateCmdImageFlag); v != "" {
						container.Image = v
						slog.Info("Set container image for container '%s' to %s from --%s", containerName, v, generateCmdImageFlag)
						workload.Containers[containerName] = container
					}
				}
			}

			if state, err = state.WithWorkload(&workload, &arg, framework.NoExtras{}); err != nil {
				return errors.Wrapf(err, "failed to add score file to project: %s", arg)
			}
			slog.Info("Added score file to project", "file", arg)
		}

		if len(state.Workloads) == 0 {
			return errors.New("Project is empty, please add a score file")
		}

		var err error
		if state, err = state.WithPrimedResources(); err != nil {
			return errors.Wrap(err, "failed to prime resources")
		}
		slog.Info("Primed resources", "#workloads", len(state.Workloads), "#resources", len(state.Resources))

		localProvisioners, err := loader.LoadProvisionersFromDirectory(projectDirectory, loader.DefaultSuffix)
		if err != nil {
			return errors.Wrapf(err, "failed to load provisioners")
		}
		slog.Info("Loaded provisioners", "#provisioners", len(localProvisioners))

		state, err = provisioners.ProvisionResources(context.Background(), state, localProvisioners)
		if err != nil {
			return errors.Wrapf(err, "failed to provision resources")
		}

		if f, err := os.OpenFile(filepath.Join(projectDirectory, stateFileName+".tmp"), os.O_CREATE|os.O_WRONLY, 0600); err != nil {
			return errors.Wrapf(err, "failed to open state file for writing")
		} else {
			defer f.Close()
			if err = yaml.NewEncoder(f).Encode(state); err != nil {
				return errors.Wrapf(err, "failed to marshal yaml to state file")
			} else if err = f.Close(); err != nil {
				return errors.Wrapf(err, "failed to close state file")
			} else if err = os.Rename(f.Name(), filepath.Join(projectDirectory, stateFileName)); err != nil {
				return errors.Wrapf(err, "failed to move new state file")
			}
			slog.Info("Persisted state file")
		}

		out := new(bytes.Buffer)
		var outCount int

		for _, manifest := range state.Extras.Manifests {
			if p, ok := internal.FindFirstUnresolvedSecretRef("", manifest); ok {
				return errors.Errorf("unresolved secret ref in manifest: %s", p)
			}
			out.WriteString("---\n")
			enc := yaml.NewEncoder(out)
			enc.SetIndent(2)
			if err := enc.Encode(manifest); err != nil {
				return errors.Wrapf(err, "failed to recode")
			}
			out.WriteString("\n")
			outCount += 1
		}
		slog.Info(fmt.Sprintf("Wrote %d resource manifests to manifests buffer", len(state.Extras.Manifests)))

		for workloadName := range state.Workloads {
			manifests, err := convert.ConvertWorkload(state, workloadName)
			if err != nil {
				return errors.Wrapf(err, "workload: %s: failed to convert", workloadName)
			}
			for _, m := range manifests {
				subOut := new(bytes.Buffer)
				if err = internal.YamlSerializerInfo.Serializer.Encode(m.(runtime.Object), subOut); err != nil {
					return errors.Wrapf(err, "workload: %s: failed to serialise manifest %s", workloadName, m.GetName())
				}
				var intermediate interface{}
				_ = yaml.Unmarshal(subOut.Bytes(), &intermediate)
				if p, ok := internal.FindFirstUnresolvedSecretRef("", intermediate); ok {
					return errors.Errorf("unresolved secret ref in manifest: %s", p)
				}
				out.WriteString("---\n")
				_, _ = subOut.WriteTo(out)
				out.WriteString("\n")
				outCount += 1
			}
			slog.Info(fmt.Sprintf("Wrote %d manifests to manifests buffer for workload '%s'", len(manifests), workloadName))
		}

		v, _ := cmd.Flags().GetString(generateCmdOutputFlag)
		if v == "" {
			return fmt.Errorf("no output file specified")
		} else if v == "-" {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), out.String())
		} else if err := os.WriteFile(v+".tmp", out.Bytes(), 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		} else if err := os.Rename(v+".tmp", v); err != nil {
			return fmt.Errorf("failed to complete writing output file: %w", err)
		} else {
			slog.Info(fmt.Sprintf("Wrote manifests to '%s'", v))
		}
		return nil
	},
}

func parseAndApplyOverrideFile(entry string, flagName string, spec map[string]interface{}) error {
	if raw, err := os.ReadFile(entry); err != nil {
		return fmt.Errorf("--%s '%s' is invalid, failed to read file: %w", flagName, entry, err)
	} else {
		slog.Info(fmt.Sprintf("Applying overrides from %s to workload", entry))
		var out map[string]interface{}
		if err := yaml.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("--%s '%s' is invalid: failed to decode yaml: %w", flagName, entry, err)
		} else if err := mergo.Merge(&spec, out, mergo.WithOverride); err != nil {
			return fmt.Errorf("--%s '%s' failed to apply: %w", flagName, entry, err)
		}
	}
	return nil
}

func parseAndApplyOverrideProperty(entry string, flagName string, spec map[string]interface{}) (map[string]interface{}, error) {
	parts := strings.SplitN(entry, "=", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("--%s '%s' is invalid, expected a =-separated path and value", flagName, entry)
	}
	if parts[1] == "" {
		slog.Info(fmt.Sprintf("Overriding '%s' in workload", parts[0]))
		after, err := framework.OverridePathInMap(spec, framework.ParseDotPathParts(parts[0]), true, nil)
		if err != nil {
			return nil, fmt.Errorf("--%s '%s' could not be applied: %w", flagName, entry, err)
		}
		return after, nil
	} else {
		var value interface{}
		if err := yaml.Unmarshal([]byte(parts[1]), &value); err != nil {
			return nil, fmt.Errorf("--%s '%s' is invalid, failed to unmarshal value as json: %w", flagName, entry, err)
		}
		slog.Info(fmt.Sprintf("Overriding '%s' in workload", parts[0]))
		after, err := framework.OverridePathInMap(spec, framework.ParseDotPathParts(parts[0]), false, value)
		if err != nil {
			return nil, fmt.Errorf("--%s '%s' could not be applied: %w", flagName, entry, err)
		}
		return after, nil
	}
}

func init() {
	generateCmd.Flags().StringP(generateCmdOutputFlag, "o", "manifests.yaml", "The output manifests file to write the manifests to")
	generateCmd.Flags().String(generateCmdOverridesFileFlag, "", "An optional file of Score overrides to merge in")
	generateCmd.Flags().StringArray(generateCmdOverridePropertyFlag, []string{}, "An optional set of path=key overrides to set or remove")
	generateCmd.Flags().String(generateCmdImageFlag, "", "An optional container image to use for any container with image == '.'")

	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(generateCmd)
	rootCmd.CompletionOptions = cobra.CompletionOptions{HiddenDefaultCmd: true}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error: "+err.Error())
		os.Exit(1)
	}
}
