/*
 * Copyright © 2025 Broadcom Inc. and/or its subsidiaries. All Rights Reserved.
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package lib

import (
	"encoding/pem"
	"strings"

	"github.com/vmware/alb-sdk/go/models"
)

// SplitPKICABundle returns the CA certificates a PKI profile should list for
// bundle: one entry per certificate when bundle is a canonical PEM sequence of
// two or more CERTIFICATE blocks (a root plus intermediates, as AKO itself
// assembles them), so each is a trusted CA of its own. Any other input — a
// single certificate, or text that is not exactly the canonical encoding of its
// blocks — is returned as the single entry it always was, so existing PKI
// profiles (route destinationCA) keep their payload and checksum.
//
// The split entries concatenate back to bundle, which keeps
// PKICACertsChecksumInput(entries) == bundle: the node checksum and the cache
// checksum stay equal and a PKI profile is not PUT on every sync.
func SplitPKICABundle(bundle string) []string {
	var certs []string
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return []string{bundle}
		}
		certs = append(certs, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})))
	}
	if len(certs) < 2 || strings.Join(certs, "") != bundle {
		return []string{bundle}
	}
	return certs
}

// PKICACertsChecksumInput is the certificate text a PKI profile's cache
// checksum is taken over: every CA certificate, concatenated in order. For the
// common single-certificate profile it is that certificate, as before.
func PKICACertsChecksumInput(caCerts []*models.SSLCertificate) string {
	var b strings.Builder
	for _, c := range caCerts {
		if c != nil && c.Certificate != nil {
			b.WriteString(*c.Certificate)
		}
	}
	return b.String()
}
