package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
)

type Options struct {
	manifestDir      string
	stripDescriptors bool
	outputFile       string
	logLevel         string
	csvName          string
}

func newRunCmd() *cobra.Command {
	o := Options{}
	cmd := &cobra.Command{
		Use:  "migrate",
		RunE: o.Run,
	}
	cmd.Flags().StringVar(&o.manifestDir, "manifests", "./manifests", "path to the manifests directory")
	cmd.Flags().StringVar(&o.outputFile, "output-file", "", "configures the output file for the generated CSV")
	cmd.Flags().StringVar(&o.logLevel, "log-level", logrus.InfoLevel.String(), "log level")
	cmd.Flags().StringVar(&o.csvName, "csv-name", "", "configures the metadata.Name of the generated CSV")
	cmd.Flags().BoolVar(&o.stripDescriptors, "strip-descriptors", true, "controls whether CRD descriptions will be stripped when processing a CRD YAML manifest")

	if err := cmd.MarkFlagRequired("csv-name"); err != nil {
		panic(err)
	}

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
	var (
		saName          string
		crRules         []rbacv1.PolicyRule
		roleRules       []rbacv1.PolicyRule
		descriptions    []operatorsv1alpha1.CRDDescription
		deploymentSpecs []operatorsv1alpha1.StrategyDeploymentSpec
	)

	logger := logrus.WithFields(logrus.Fields{
		"manifestDir": o.manifestDir,
		"outputFile":  o.outputFile,
	})
	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("failed to parse the %s log level: %v", o.logLevel, err)
	}
	logger.Logger.Level = level

	apiextensionsv1.AddToScheme(scheme.Scheme)
	decoder := scheme.Codecs.UniversalDeserializer()

	fsys := os.DirFS(o.manifestDir)
	csv := &operatorsv1alpha1.ClusterServiceVersion{}
	csv.TypeMeta = metav1.TypeMeta{
		APIVersion: operatorsv1alpha1.ClusterServiceVersionAPIVersion,
		Kind:       operatorsv1alpha1.ClusterServiceVersionKind,
	}
	csv.SetName(o.csvName)

	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if filepath.Ext(path) != ".yaml" {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		// Note: this is a super hacky way of ensuring we can still process multi-document
		// YAML manifests, and working around CRD descriptors that contain field descriptors
		// that contain the "---" separating character that controller-gen will populate.
		// TODO: there's likely a much better implementation but that would require using my brain.
		dataStr := string(data)
		if o.stripDescriptors && strings.Contains(string(data), "CustomResourceDefinition") {
			dataStr = strings.ReplaceAll(dataStr, "---", "")
		}
		resources := strings.Split(dataStr, "---")

		for _, resource := range resources {
			if len(resource) == 0 {
				continue
			}
			obj, gvk, err := decoder.Decode([]byte(resource), nil, nil)
			if err != nil {
				logger.Warnf("failed to decode manifest", path)
				continue
			}

			switch gvk.Kind {
			case "Deployment":
				deployment, ok := obj.(*appsv1.Deployment)
				if !ok {
					continue
				}
				deploymentSpecs = append(deploymentSpecs, operatorsv1alpha1.StrategyDeploymentSpec{
					Name: deployment.GetName(),
					Spec: deployment.Spec,
				})
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

	// TODO: clean this implementation up
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
	if len(deploymentSpecs) != 0 {
		csv.Spec.InstallStrategy.StrategyName = "deployment"
		csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs = deploymentSpecs
	}

	outputFile := os.Stdout
	if o.outputFile != "" {
		_, err := os.Stat(o.outputFile)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("failed to open the configured output file (%s): %v", o.outputFile, err)
			}
		}
		outputFile, err = os.Create(o.outputFile)
		if err != nil {
			return err
		}
	}
	defer outputFile.Close()

	// TODO: handle case where empty fields are being encoded
	logger.Debugf("creating the generated CSV at the %v file", outputFile.Name())
	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
	if err := s.Encode(csv, outputFile); err != nil {
		return err
	}

	return nil
}
