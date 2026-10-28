package extract

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dag-andersen/argocd-diff-preview/pkg/argoapplication"
	"github.com/dag-andersen/argocd-diff-preview/pkg/argocd"
	"github.com/dag-andersen/argocd-diff-preview/pkg/git"
	"github.com/google/uuid"
)

// RenderApplicationsViaAPI renders committed sources without a Kubernetes
// client or controller reconciliation. Namespace normalization is intentionally
// omitted: the manifests API returns raw resources, not discovery metadata.
func RenderApplicationsViaAPI(client *argocd.APIOnlyClient, timeout uint64, concurrency uint,
	baseApps, targetApps []argoapplication.ArgoResource,
) ([]ExtractedApp, []ExtractedApp, time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	apps := append(append([]argoapplication.ArgoResource{}, baseApps...), targetApps...)
	if err := VerifyNoApplicationSets(apps); err != nil {
		return nil, nil, time.Since(start), err
	}
	if len(apps) == 0 {
		return nil, nil, time.Since(start), nil
	}
	workers := int(concurrency)
	if workers == 0 || workers > len(apps) {
		workers = len(apps)
	}
	jobs := make(chan int, len(apps))
	for i := range apps {
		jobs <- i
	}
	close(jobs)
	results := make([]ExtractedApp, len(apps))
	errs := make([]error, len(apps))
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				if err := ctx.Err(); err != nil {
					errs[i] = err
					continue
				}
				results[i], errs[i] = renderApplicationViaAPI(ctx, client, apps[i])
			}
		})
	}
	wg.Wait() // Includes cleanup, including after rendering fails.
	var base, target []ExtractedApp
	for i, result := range results {
		if errs[i] != nil {
			continue
		}
		switch result.Branch {
		case git.Base:
			base = append(base, result)
		case git.Target:
			target = append(target, result)
		default:
			errs[i] = fmt.Errorf("unknown branch type %q", result.Branch)
		}
	}
	return base, target, time.Since(start), errors.Join(errs...)
}

func renderApplicationViaAPI(ctx context.Context, client *argocd.APIOnlyClient, app argoapplication.ArgoResource) (result ExtractedApp, err error) {
	app.Yaml = app.Yaml.DeepCopy()
	name := "diff-preview-" + uuid.NewString()
	app.Yaml.SetName(name)
	prepared, err := client.PrepareApplication(app.Yaml)
	if err != nil {
		return result, err
	}
	if err = client.CreateApplication(ctx, prepared); err != nil {
		// A failed create can be ambiguous (e.g. a lost response). Report the
		// name for operator cleanup; never delete an unconfirmed creation.
		return result, fmt.Errorf("create %s (check for a leftover Application): %w", name, err)
	}
	defer func() {
		// Cleanup must run even when the rendering deadline has expired.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := client.DeleteApplication(cleanupCtx, name); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("delete preview Application %s: %w", name, cleanupErr))
		}
	}()
	manifests, err := client.Manifests(ctx, name)
	if err != nil {
		return result, fmt.Errorf("render %s: %w", app.GetLongName(), err)
	}
	replaceAppIdInManifests(manifests, name, app.Name)
	ApplyIgnoreDifferences(manifests, app)
	if err := removeArgoCDTrackingID(manifests); err != nil {
		return result, err
	}
	return CreateExtractedApp(app.Id, app.Name, app.FileName, manifests, app.Branch), nil
}
