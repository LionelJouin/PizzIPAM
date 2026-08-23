/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command celcheck validates the CEL in PizzIPAM's deployment manifests the same
// way the API server does at apply time -- WITHOUT a cluster.
//
// It catches, offline and in CI:
//   - CustomResourceDefinition x-kubernetes-validations that exceed the apiserver's
//     STATIC CEL cost budget (the "estimated rule cost exceeds budget" apply error),
//     or that fail to compile / type-check.
//   - ValidatingAdmissionPolicy variables and validation expressions that fail to
//     compile (references to variables.* included).
//   - MutatingAdmissionPolicy variables and mutation (JSONPatch / ApplyConfiguration)
//     expressions that fail to compile.
//
// It does NOT execute the CEL -- runtime allocation behavior is covered by the e2e
// suite. This lives in its own Go module so the apiserver validator/compiler deps
// (heavy, and irrelevant to consumers of apis/) never leak into the root module.
// Keep its k8s.io/* versions in step with the root go.mod on version bumps.
//
// Usage: celcheck <path>...  (files or directories; default ./deployment).
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"
)

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"deployment"}
	}
	files, err := gatherYAML(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "error: no YAML files found in", paths)
		os.Exit(2)
	}

	total, failed := 0, 0
	for _, f := range files {
		docs, err := splitDocs(f)
		if err != nil {
			fmt.Printf("FAIL %s: %v\n", f, err)
			failed++
			continue
		}
		for _, doc := range docs {
			kind, name := identify(doc)
			var errs []string
			switch kind {
			case "CustomResourceDefinition":
				errs = checkCRD(doc)
			case "ValidatingAdmissionPolicy":
				errs = checkVAP(doc)
			case "MutatingAdmissionPolicy":
				errs = checkMAP(doc)
			default:
				continue // bindings and other kinds carry no CEL to check
			}
			total++
			label := fmt.Sprintf("%s/%s (%s)", kind, name, filepath.Base(f))
			if len(errs) == 0 {
				fmt.Printf("ok   %s\n", label)
				continue
			}
			failed++
			fmt.Printf("FAIL %s\n", label)
			for _, e := range errs {
				fmt.Printf("       - %s\n", e)
			}
		}
	}

	fmt.Printf("\n%d CEL-bearing object(s) checked, %d failed\n", total, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// gatherYAML expands files and directories into a flat list of .yaml/.yml paths.
func gatherYAML(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if n := e.Name(); strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
				out = append(out, filepath.Join(p, n))
			}
		}
	}
	return out, nil
}

// splitDocs reads a possibly multi-document YAML file into individual documents.
func splitDocs(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := utilyaml.NewYAMLReader(bufio.NewReader(f))
	var docs [][]byte
	for {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(doc))) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func identify(doc []byte) (kind, name string) {
	var tm struct {
		Kind     string            `json:"kind"`
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	_ = yaml.Unmarshal(doc, &tm)
	return tm.Kind, tm.Metadata.Name
}

// checkCRD runs the real apiextensions validator, which includes the static CEL
// cost-budget estimation and compilation of every x-kubernetes-validations rule.
func checkCRD(doc []byte) []string {
	var v1crd apiextv1.CustomResourceDefinition
	if err := yaml.Unmarshal(doc, &v1crd); err != nil {
		return []string{"unmarshal: " + err.Error()}
	}
	var internal apiext.CustomResourceDefinition
	if err := apiextv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1crd, &internal, nil); err != nil {
		return []string{"convert: " + err.Error()}
	}
	// Populate status so validation surfaces only rule / cost errors, not
	// name-acceptance bookkeeping.
	if len(internal.Spec.Versions) > 0 {
		internal.Status.StoredVersions = []string{internal.Spec.Versions[0].Name}
	}
	internal.Status.AcceptedNames = internal.Spec.Names

	var out []string
	for _, e := range crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal) {
		out = append(out, e.Error())
	}
	return out
}

func checkVAP(doc []byte) []string {
	var vap admissionv1.ValidatingAdmissionPolicy
	if err := yaml.Unmarshal(doc, &vap); err != nil {
		return []string{"unmarshal: " + err.Error()}
	}
	cc, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		return []string{"init compiler: " + err.Error()}
	}
	opts := plugincel.OptionalVariableDeclarations{
		HasParams:     vap.Spec.ParamKind != nil,
		HasAuthorizer: true,
	}
	var out []string
	// Variables must be compiled (and stored) in declared order so later
	// expressions can reference variables.<name>.
	for _, v := range vap.Spec.Variables {
		res := cc.CompileAndStoreVariable(&validating.Variable{Name: v.Name, Expression: v.Expression}, opts, environment.StoredExpressions)
		if res.Error != nil {
			out = append(out, fmt.Sprintf("variable %q: %s", v.Name, res.Error.Error()))
		}
	}
	for i, val := range vap.Spec.Validations {
		res := cc.CompileCELExpression(&validating.ValidationCondition{Expression: val.Expression}, opts, environment.StoredExpressions)
		if res.Error != nil {
			out = append(out, fmt.Sprintf("validation[%d]: %s", i, res.Error.Error()))
		}
	}
	return out
}

func checkMAP(doc []byte) []string {
	var mp admissionv1.MutatingAdmissionPolicy
	if err := yaml.Unmarshal(doc, &mp); err != nil {
		return []string{"unmarshal: " + err.Error()}
	}
	cc, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		return []string{"init compiler: " + err.Error()}
	}
	opts := plugincel.OptionalVariableDeclarations{
		HasParams:     mp.Spec.ParamKind != nil,
		HasAuthorizer: true,
	}
	var out []string
	for _, v := range mp.Spec.Variables {
		res := cc.CompileAndStoreVariable(&validating.Variable{Name: v.Name, Expression: v.Expression}, opts, environment.StoredExpressions)
		if res.Error != nil {
			out = append(out, fmt.Sprintf("variable %q: %s", v.Name, res.Error.Error()))
		}
	}
	// Mutation expressions additionally get the JSONPatch / Object CEL types.
	patchOpts := opts
	patchOpts.HasPatchTypes = true
	for i, m := range mp.Spec.Mutations {
		switch m.PatchType {
		case admissionv1.PatchTypeJSONPatch:
			if m.JSONPatch == nil {
				continue
			}
			res := cc.CompileCELExpression(&patch.JSONPatchCondition{Expression: m.JSONPatch.Expression}, patchOpts, environment.StoredExpressions)
			if res.Error != nil {
				out = append(out, fmt.Sprintf("mutation[%d] jsonPatch: %s", i, res.Error.Error()))
			}
		case admissionv1.PatchTypeApplyConfiguration:
			if m.ApplyConfiguration == nil {
				continue
			}
			res := cc.CompileCELExpression(&patch.ApplyConfigurationCondition{Expression: m.ApplyConfiguration.Expression}, patchOpts, environment.StoredExpressions)
			if res.Error != nil {
				out = append(out, fmt.Sprintf("mutation[%d] applyConfiguration: %s", i, res.Error.Error()))
			}
		}
	}
	return out
}
