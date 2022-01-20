package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
)

type Options struct {
	manifestDir string
}

func newRunCmd() *cobra.Command {
	o := Options{}
	cmd := &cobra.Command{
		Use:  "migrate",
		RunE: o.Run,
	}
	cmd.Flags().StringVar(&o.manifestDir, "manifests", "./manifests", "path to the manifests directory")

	return cmd
}

func main() {
	cmd := newRunCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (o *Options) Run(cmd *cobra.Command, args []string) error {
	csv := &operatorsv1alpha1.ClusterServiceVersion{}

	var (
		saName       string
		crRules      []rbacv1.PolicyRule
		roleRules    []rbacv1.PolicyRule
		descriptions = []operatorsv1alpha1.CRDDescription{}
	)

	apiextensionsv1.AddToScheme(scheme.Scheme)
	decoder := scheme.Codecs.UniversalDeserializer()

	fsys := os.DirFS(o.manifestDir)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if filepath.Ext(path) != ".yaml" {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		for _, resource := range strings.Split(string(data), "---") {
			if len(resource) == 0 {
				continue
			}
			obj, gvk, err := decoder.Decode([]byte(resource), nil, nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, "failed to decode manifest", path)
				continue
			}

			switch gvk.Kind {
			case "Deployment":
				deployment, ok := obj.(*appsv1.Deployment)
				if !ok {
					continue
				}
				csv.Spec.InstallStrategy.StrategyName = "deployment"
				csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs = []operatorsv1alpha1.StrategyDeploymentSpec{
					{
						Name: deployment.GetName(),
						Spec: deployment.Spec,
					},
				}
			case "ServiceAccount":
				sa, ok := obj.(*corev1.ServiceAccount)
				if !ok {
					continue
				}
				saName = sa.GetName()
			case "ClusterRole":
				cr, ok := obj.(*rbacv1.ClusterRole)
				if !ok {
					continue
				}
				crRules = append(crRules, cr.Rules...)
			case "Role":
				role, ok := obj.(*rbacv1.Role)
				if !ok {
					continue
				}
				roleRules = append(roleRules, role.Rules...)
			case "CustomResourceDefinition":
				crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
				if !ok {
					continue
				}
				fmt.Println("this ran for the CRD path", path)
				descriptions = append(descriptions, operatorsv1alpha1.CRDDescription{
					Name:    crd.Name,
					Version: crd.APIVersion,
					Kind:    crd.Kind,
				})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if saName == "" {
		return fmt.Errorf("validation: unable to find a ServiceAccount manifest in the %s directory", o.manifestDir)
	}

	if len(crRules) != 0 {
		csv.Spec.InstallStrategy.StrategySpec.ClusterPermissions = []operatorsv1alpha1.StrategyDeploymentPermissions{
			{
				ServiceAccountName: saName,
				Rules:              crRules,
			},
		}
	}
	if len(roleRules) != 0 {
		csv.Spec.InstallStrategy.StrategySpec.Permissions = []operatorsv1alpha1.StrategyDeploymentPermissions{
			{
				ServiceAccountName: saName,
				Rules:              roleRules,
			},
		}
	}
	if len(descriptions) != 0 {
		csv.Spec.CustomResourceDefinitions = operatorsv1alpha1.CustomResourceDefinitions{
			Owned: descriptions,
		}
	}

	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
	if err := s.Encode(csv, os.Stdout); err != nil {
		return err
	}

	return nil
}
