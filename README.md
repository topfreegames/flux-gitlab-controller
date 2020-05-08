# sample-controller

This repository implements a simple controller for watching Secrets created by gitlab and calling the API to update the deploy keys the specified repo

**Note:** go-get or vendor this package as `github.com/topfreegames/flux-gitlab-controller`.

## Details

The sample controller uses [client-go library](https://github.com/kubernetes/client-go/tree/master/tools/cache) extensively.

## Running

**Prerequisite**: Since the controller uses `apps/v1` deployments, the Kubernetes cluster version should be greater than 1.9.

```sh
# assumes you have a working kubeconfig, not required if operating in-cluster
go build
./flux-gitlab-controller -gitlab-token $TOKEN -kubeconfig=$HOME/.kube/config

# create a flux secret with the corresponding `fluxcd.io/git-url` and `fluxcd.io/sync-gc-mark` marks
kubectl create -f artifacts/examples/flux_secret.yaml

# Check that the fluxcd.io/deployKeyId has been created in the secret and that the repo contains
# the associated deployment key
kubectl get secret -o yaml flux-git-deploy
```

## What happens if someone removes the deployment key from the application repo?

In tha case, flux won't re-create the key as we're not constantly checking for deleted keys to avoid
putting too much pressure to the gitlab api. 

In order for flux to re-create the key, the fluxcd.io/deployKeyId annotation needs to be removed
from the secret so flux realizes that the secret is not synched and will recreate the appropriate key
