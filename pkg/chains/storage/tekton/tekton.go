/*
Copyright 2020 The Tekton Authors
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

package tekton

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/tektoncd/chains/pkg/chains/formats"

	"github.com/tektoncd/chains/pkg/config"

	"github.com/tektoncd/chains/pkg/patch"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	versioned "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	"go.uber.org/zap"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	StorageBackendTekton        = "tekton"
	TaskRunAnnotationFormat     = "chains.tekton.dev/taskrun-%s"     // payload type
	AttestationAnnotationFormat = "chains.tekton.dev/attestation-%s" // payload type
	SignatureAnnotationFormat   = "chains.tekton.dev/signature-%s"
	CertAnnotationsFormat       = "chains.tekton.dev/cert-%s"
	ChainAnnotationFormat       = "chains.tekton.dev/chain-%s"
)

// Backend is a storage backend that stores signed payloads in the TaskRun metadata as an annotation.
// It is stored as base64 encoded JSON.
type Backend struct {
	pipelienclientset versioned.Interface
	logger            *zap.SugaredLogger
	tr                *v1beta1.TaskRun
}

// NewStorageBackend returns a new Tekton StorageBackend that stores signatures on a TaskRun
func NewStorageBackend(ps versioned.Interface, logger *zap.SugaredLogger, tr *v1beta1.TaskRun) *Backend {
	return &Backend{
		pipelienclientset: ps,
		logger:            logger,
		tr:                tr,
	}
}

// StorePayload implements the Payloader interface.
func (b *Backend) StorePayload(rawPayload []byte, signature string, opts config.StorageOpts) error {
	b.logger.Infof("Storing payload on TaskRun %s/%s", b.tr.Namespace, b.tr.Name)

	patchMap := map[string]string{
		fmt.Sprintf(SignatureAnnotationFormat, opts.Key): base64.StdEncoding.EncodeToString([]byte(signature)),
		fmt.Sprintf(CertAnnotationsFormat, opts.Key):     base64.StdEncoding.EncodeToString([]byte(opts.Cert)),
		fmt.Sprintf(ChainAnnotationFormat, opts.Key):     base64.StdEncoding.EncodeToString([]byte(opts.Chain)),
	}
	payloadKey, err := b.payloadKey(opts)
	if err != nil {
		return err
	}
	patchMap[payloadKey] = base64.StdEncoding.EncodeToString([]byte(rawPayload))

	// Use patch instead of update to prevent race conditions.
	patchBytes, err := patch.GetAnnotationsPatch(patchMap)
	if err != nil {
		return err
	}
	if _, err := b.pipelienclientset.TektonV1beta1().TaskRuns(b.tr.Namespace).Patch(
		context.TODO(), b.tr.Name, types.MergePatchType, patchBytes, v1.PatchOptions{}); err != nil {
		return err
	}
	return nil
}

func (b *Backend) Type() string {
	return StorageBackendTekton
}

// retrieveAnnotationValue retrieve the value of an annotation and base64 decode it if needed.
func (b *Backend) retrieveAnnotationValue(annotationKey string, decode bool) (string, error) {
	// Retrieve the TaskRun.
	b.logger.Infof("Retrieving annotation %q on TaskRun %s/%s", annotationKey, b.tr.Namespace, b.tr.Name)
	tr, err := b.pipelienclientset.TektonV1beta1().TaskRuns(b.tr.Namespace).Get(context.TODO(), b.tr.Name, v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error retrieving taskrun: %s", err)
	}

	// Retrieve the annotation.
	var annotationValue string
	rawAnnotationValue, exists := tr.Annotations[annotationKey]

	// Ensure it exists.
	if exists {
		// Decode it if needed.
		if decode {
			decodedAnnotation, err := base64.StdEncoding.DecodeString(rawAnnotationValue)
			if err != nil {
				return "", fmt.Errorf("error decoding the annotation value for the key %q: %s", annotationKey, err)
			}
			annotationValue = string(decodedAnnotation)
		} else {
			annotationValue = rawAnnotationValue
		}
	}

	return annotationValue, nil
}

// RetrieveSignature retrieve the signature stored in the taskrun.
func (b *Backend) RetrieveSignature(opts config.StorageOpts) (string, error) {
	b.logger.Infof("Retrieving signature on TaskRun %s/%s", b.tr.Namespace, b.tr.Name)
	return b.retrieveAnnotationValue(fmt.Sprintf(SignatureAnnotationFormat, opts.Key), true)
}

// RetrievePayload retrieve the payload stored in the taskrun.
func (b *Backend) RetrievePayload(opts config.StorageOpts) (string, error) {
	b.logger.Infof("Retrieving payload on TaskRun %s/%s", b.tr.Namespace, b.tr.Name)
	payloadKey, err := b.payloadKey(opts)
	if err != nil {
		return "", err
	}
	payload, err := b.retrieveAnnotationValue(payloadKey, true)
	if err != nil {
		return "", err
	}

	return string(payload), nil
}

func (b *Backend) payloadKey(opts config.StorageOpts) (string, error) {
	if opts.PayloadFormat == formats.PayloadTypeTekton {
		return fmt.Sprintf(TaskRunAnnotationFormat, opts.Key), nil
	} else if opts.PayloadFormat == formats.PayloadTypeInTotoIte6 {
		return fmt.Sprintf(AttestationAnnotationFormat, opts.Key), nil
	} else {
		return "", fmt.Errorf("tekton storage does not support payloads of type %q", opts.PayloadFormat)
	}
}
