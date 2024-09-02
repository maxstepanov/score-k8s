// Copyright 2024 Humanitec
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmdprov

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/score-spec/score-go/framework"
	"gopkg.in/yaml.v3"

	"github.com/score-spec/score-k8s/internal/provisioners"
)

type Provisioner struct {
	ProvisionerUri string   `yaml:"uri"`
	ResType        string   `yaml:"type"`
	ResClass       *string  `yaml:"class,omitempty"`
	ResId          *string  `yaml:"id,omitempty"`
	Args           []string `yaml:"args"`
}

func (p *Provisioner) Uri() string {
	return p.ProvisionerUri
}

func (p *Provisioner) Match(resUid framework.ResourceUid) bool {
	if resUid.Type() != p.ResType {
		return false
	} else if p.ResClass != nil && resUid.Class() != *p.ResClass {
		return false
	} else if p.ResId != nil && resUid.Id() != *p.ResId {
		return false
	}
	return true
}

func decodeBinary(uri string) (string, error) {
	parts, _ := url.Parse(uri)
	pathParts := strings.Split(parts.EscapedPath(), "/")
	switch parts.Hostname() {
	case "":
		return string(filepath.Separator) + filepath.Join(pathParts...), nil
	case "~":
		hd, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve user home directory: %w", err)
		}
		pathParts = slices.Insert(pathParts, 0, hd)
	case ".":
		pwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to resolve current working directory: %w", err)
		}
		pathParts = slices.Insert(pathParts, 0, pwd)
	case "..":
		pwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to resolve current working directory: %w", err)
		}
		pathParts = slices.Insert(pathParts, 0, filepath.Dir(pwd))
	default:
		if len(pathParts) > 1 {
			return "", fmt.Errorf("direct command reference cannot contain additional path parts")
		}
		b, err := exec.LookPath(parts.Hostname())
		if err != nil {
			return "", fmt.Errorf("failed to find '%s' on path: %w", parts.Hostname(), err)
		}
		pathParts = slices.Insert(pathParts, 0, b)
	}
	return filepath.Join(pathParts...), nil
}

func (p *Provisioner) Provision(ctx context.Context, input *provisioners.Input) (*provisioners.ProvisionOutput, error) {
	data := provisioners.TemplateData{
		Guid:             input.ResourceGuid,
		Uid:              input.ResourceUid,
		Type:             input.ResourceType,
		Class:            input.ResourceClass,
		Id:               input.ResourceId,
		Params:           input.ResourceParams,
		Metadata:         input.ResourceMetadata,
		State:            input.ResourceState,
		Shared:           input.SharedState,
		SourceWorkload:   input.SourceWorkload,
		WorkloadServices: input.WorkloadServices,
	}

	bin, err := decodeBinary(p.Uri())
	if err != nil {
		return nil, err
	}

	rawInput, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to encode json input: %w", err)
	}
	outputBuffer := new(bytes.Buffer)

	// if there is a <mode> arg, we mark it as "provision".
	args := slices.Clone(p.Args)
	for i, arg := range args {
		if arg == "<mode>" {
			args[i] = "provision"
		}
		rendered, err := provisioners.RenderTemplate(arg, data)
		if err != nil {
			return nil, err
		}
		args[i] = rendered
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	slog.Debug(fmt.Sprintf("Executing '%s %v' for command provisioner", bin, args))
	cmd.Stdin = bytes.NewReader(rawInput)
	cmd.Stdout = outputBuffer
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute cmd provisioner: %w", err)
	}

	var output provisioners.ProvisionOutput
	dec := json.NewDecoder(bytes.NewReader(outputBuffer.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&output); err != nil {
		slog.Debug("Output from command provisioner:\n" + outputBuffer.String())
		return nil, fmt.Errorf("failed to decode output from cmd provisioner: %w", err)
	}

	return &output, nil
}

func Parse(raw map[string]interface{}) (*Provisioner, error) {
	p := new(Provisioner)
	intermediate, _ := yaml.Marshal(raw)
	dec := yaml.NewDecoder(bytes.NewReader(intermediate))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.ProvisionerUri == "" {
		return nil, fmt.Errorf("uri not set")
	} else if p.ResType == "" {
		return nil, fmt.Errorf("type not set")
	}

	parts, err := url.Parse(p.ProvisionerUri)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %w", err)
	} else if parts.User != nil {
		return nil, fmt.Errorf("cmd provisioner uri cannot contain user info")
	} else if len(parts.Query()) != 0 {
		return nil, fmt.Errorf("cmd provisioner uri cannot contain query params")
	} else if parts.Port() != "" {
		return nil, fmt.Errorf("cmd provisioner uri cannot contain a port")
	}

	return p, nil
}
