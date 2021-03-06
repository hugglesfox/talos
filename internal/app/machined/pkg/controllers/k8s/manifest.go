// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package k8s

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"

	"github.com/AlekSi/pointer"
	"github.com/talos-systems/os-runtime/pkg/controller"
	"github.com/talos-systems/os-runtime/pkg/resource"
	"github.com/talos-systems/os-runtime/pkg/state"

	"github.com/talos-systems/talos/internal/app/machined/pkg/resources/config"
	"github.com/talos-systems/talos/internal/app/machined/pkg/resources/k8s"
	"github.com/talos-systems/talos/internal/app/machined/pkg/resources/secrets"
)

// ManifestController renders manifests based on templates and config/secrets.
type ManifestController struct{}

// Name implements controller.Controller interface.
func (ctrl *ManifestController) Name() string {
	return "k8s.ManifestController"
}

// ManagedResources implements controller.Controller interface.
func (ctrl *ManifestController) ManagedResources() (resource.Namespace, resource.Type) {
	return k8s.ControlPlaneNamespaceName, k8s.ManifestType
}

// Run implements controller.Controller interface.
//
//nolint: gocyclo
func (ctrl *ManifestController) Run(ctx context.Context, r controller.Runtime, logger *log.Logger) error {
	if err := r.UpdateDependencies([]controller.Dependency{
		{
			Namespace: config.NamespaceName,
			Type:      config.K8sControlPlaneType,
			ID:        pointer.ToString(config.K8sManifestsID),
			Kind:      controller.DependencyWeak,
		},
		{
			Namespace: secrets.NamespaceName,
			Type:      secrets.KubernetesType,
			ID:        pointer.ToString(secrets.KubernetesID),
			Kind:      controller.DependencyWeak,
		},
	}); err != nil {
		return fmt.Errorf("error setting up dependencies: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		configResource, err := r.Get(ctx, resource.NewMetadata(config.NamespaceName, config.K8sControlPlaneType, config.K8sManifestsID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				if err = ctrl.teardownAll(ctx, r); err != nil {
					return fmt.Errorf("error tearing down: %w", err)
				}

				continue
			}

			return err
		}

		config := configResource.(*config.K8sControlPlane).Manifests()

		secretsResources, err := r.Get(ctx, resource.NewMetadata(secrets.NamespaceName, secrets.KubernetesType, secrets.KubernetesID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				if err = ctrl.teardownAll(ctx, r); err != nil {
					return fmt.Errorf("error tearing down: %w", err)
				}

				continue
			}

			return err
		}

		secrets := secretsResources.(*secrets.Kubernetes).Secrets()

		renderedManifests, err := ctrl.render(config, *secrets)
		if err != nil {
			return err
		}

		for _, renderedManifest := range renderedManifests {
			renderedManifest := renderedManifest

			if err = r.Update(ctx, k8s.NewManifest(k8s.ControlPlaneNamespaceName, renderedManifest.name),
				func(r resource.Resource) error {
					return r.(*k8s.Manifest).SetYAML(renderedManifest.data)
				}); err != nil {
				return fmt.Errorf("error updating manifests: %w", err)
			}
		}

		// remove any manifests which weren't rendered
		manifests, err := r.List(ctx, resource.NewMetadata(k8s.ControlPlaneNamespaceName, k8s.ManifestType, "", resource.VersionUndefined))
		if err != nil {
			return fmt.Errorf("error listing manifests: %w", err)
		}

		manifestsToDelete := map[string]struct{}{}

		for _, manifest := range manifests.Items {
			manifestsToDelete[manifest.Metadata().ID()] = struct{}{}
		}

		for _, renderedManifest := range renderedManifests {
			delete(manifestsToDelete, renderedManifest.name)
		}

		for id := range manifestsToDelete {
			if err = r.Destroy(ctx, resource.NewMetadata(k8s.ControlPlaneNamespaceName, k8s.ManifestType, id, resource.VersionUndefined)); err != nil {
				return fmt.Errorf("error cleaning up manifests: %w", err)
			}
		}
	}
}

type renderedManifest struct {
	name string
	data []byte
}

func (ctrl *ManifestController) render(cfg config.K8sManifestsSpec, scrt secrets.KubernetesSpec) ([]renderedManifest, error) {
	templateConfig := struct {
		config.K8sManifestsSpec

		Secrets secrets.KubernetesSpec
	}{
		K8sManifestsSpec: cfg,
		Secrets:          scrt,
	}

	type manifestDesc struct {
		name     string
		template []byte
	}

	defaultManifests := []manifestDesc{
		{"00-kubelet-bootstrapping-token", kubeletBootstrappingToken},
		{"01-csr-node-bootstrap", csrNodeBootstrapTemplate},
		{"01-csr-approver-role-binding", csrApproverRoleBindingTemplate},
		{"01-csr-renewal-role-binding", csrRenewalRoleBindingTemplate},
		{"02-kube-system-sa-role-binding", kubeSystemSARoleBindingTemplate},
		{"03-default-pod-security-policy", podSecurityPolicy},
		{"10-kube-proxy", kubeProxyTemplate},
		{"11-kube-config-in-cluster", kubeConfigInClusterTemplate},
		{"11-core-dns", coreDNSTemplate},
		{"11-core-dns-svc", coreDNSSvcTemplate},
	}

	if cfg.DNSServiceIPv6 != "" {
		defaultManifests = append(defaultManifests,
			[]manifestDesc{
				{"11-core-dns-v6-svc", coreDNSv6SvcTemplate},
			}...,
		)
	}

	if cfg.FlannelEnabled {
		defaultManifests = append(defaultManifests,
			[]manifestDesc{
				{"05-flannel", flannelTemplate},
			}...,
		)
	}

	manifests := make([]renderedManifest, len(defaultManifests))

	for i := range defaultManifests {
		tmpl, err := template.New(defaultManifests[i].name).Parse(string(defaultManifests[i].template))
		if err != nil {
			return nil, fmt.Errorf("error parsing manifest template %q: %w", defaultManifests[i].name, err)
		}

		var buf bytes.Buffer

		if err = tmpl.Execute(&buf, &templateConfig); err != nil {
			return nil, fmt.Errorf("error executing template %q: %w", defaultManifests[i].name, err)
		}

		manifests[i].name = defaultManifests[i].name
		manifests[i].data = buf.Bytes()
	}

	return manifests, nil
}

func (ctrl *ManifestController) teardownAll(ctx context.Context, r controller.Runtime) error {
	manifests, err := r.List(ctx, resource.NewMetadata(k8s.ControlPlaneNamespaceName, k8s.ManifestType, "", resource.VersionUndefined))
	if err != nil {
		return fmt.Errorf("error listing manifests: %w", err)
	}

	for _, manifest := range manifests.Items {
		if err = r.Destroy(ctx, manifest.Metadata()); err != nil {
			return fmt.Errorf("error destroying manifest: %w", err)
		}
	}

	return nil
}
