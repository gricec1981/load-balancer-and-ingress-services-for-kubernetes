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

package aigateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	avimodels "github.com/vmware/alb-sdk/go/models"
	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ─── Cache types ─────────────────────────────────────────────────────────────

// JWTProfileCacheEntry holds the Avi uuid and content checksum of an
// AKO-managed JWTServerProfile so we can skip writes when nothing changed.
type JWTProfileCacheEntry struct {
	Name             string
	UUID             string
	CloudConfigCksum string
}

// SSOPolicyCacheEntry holds the Avi uuid and content checksum of an
// AKO-managed SSO Policy.
type SSOPolicyCacheEntry struct {
	Name             string
	UUID             string
	CloudConfigCksum string
}

// jwtCache is the process-wide in-memory cache for AKO-managed JWT objects.
type jwtObjectCache struct {
	mu          sync.RWMutex
	jwtProfiles map[string]*JWTProfileCacheEntry // key: "tenant/name"
	ssoPolicies map[string]*SSOPolicyCacheEntry  // key: "tenant/name"
}

var (
	globalJWTCache     *jwtObjectCache
	jwtCacheOnce       sync.Once
)

func sharedJWTCache() *jwtObjectCache {
	jwtCacheOnce.Do(func() {
		globalJWTCache = &jwtObjectCache{
			jwtProfiles: make(map[string]*JWTProfileCacheEntry),
			ssoPolicies: make(map[string]*SSOPolicyCacheEntry),
		}
	})
	return globalJWTCache
}

func (c *jwtObjectCache) getJWTProfile(tenant, name string) (*JWTProfileCacheEntry, bool) {
	c.mu.RLock(); defer c.mu.RUnlock()
	e, ok := c.jwtProfiles[tenant+"/"+name]
	return e, ok
}
func (c *jwtObjectCache) setJWTProfile(tenant string, e *JWTProfileCacheEntry) {
	c.mu.Lock(); defer c.mu.Unlock()
	c.jwtProfiles[tenant+"/"+e.Name] = e
}
func (c *jwtObjectCache) deleteJWTProfile(tenant, name string) {
	c.mu.Lock(); defer c.mu.Unlock()
	delete(c.jwtProfiles, tenant+"/"+name)
}
func (c *jwtObjectCache) getSSOPolicy(tenant, name string) (*SSOPolicyCacheEntry, bool) {
	c.mu.RLock(); defer c.mu.RUnlock()
	e, ok := c.ssoPolicies[tenant+"/"+name]
	return e, ok
}
func (c *jwtObjectCache) setSSOPolicy(tenant string, e *SSOPolicyCacheEntry) {
	c.mu.Lock(); defer c.mu.Unlock()
	c.ssoPolicies[tenant+"/"+e.Name] = e
}
func (c *jwtObjectCache) deleteSSOPolicy(tenant, name string) {
	c.mu.Lock(); defer c.mu.Unlock()
	delete(c.ssoPolicies, tenant+"/"+name)
}

// ─── Name derivation ─────────────────────────────────────────────────────────

// jwtProfileName returns the Avi JWTServerProfile name for a policy.
func jwtProfileName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-jwt", policy.Namespace, policy.Name)
}

// ─── JWKS fetch ──────────────────────────────────────────────────────────────

// fetchJWKS fetches the JWKS JSON from jwksUri and returns the raw bytes.
// Uses a 10-second timeout; the caller caches the result by checksum.
func fetchJWKS(jwksUri string) (string, error) {
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(jwksUri)
	if err != nil {
		return "", fmt.Errorf("JWKS fetch %s: %w", jwksUri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("JWKS fetch %s: HTTP %d", jwksUri, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("JWKS read %s: %w", jwksUri, err)
	}
	// Validate it's actually JSON.
	var probe interface{}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("JWKS parse %s: %w", jwksUri, err)
	}
	return string(body), nil
}

// ─── JWTServerProfile lifecycle ──────────────────────────────────────────────

// EnsureJWTServerProfile creates or updates the Avi JWTServerProfile for the
// given AIGatewayAuthPolicy. It returns the Avi API path that the SSO Policy
// should reference, e.g. "/api/jwtserverprofile?name=inference-llm-auth-jwt".
//
// The profile stores the raw JWKS keyset fetched from spec.jwt.jwksUri.
// If jwksUri is empty no remote fetch is performed and JwksKeys is left empty
// (valid for issuers where Avi can discover the JWKS via the issuer URL).
func EnsureJWTServerProfile(key string, policy *AIGatewayAuthPolicy) (string, error) {
	name := jwtProfileName(policy)
	tenant := lib.GetTenant()
	spec := policy.Spec.JWT

	jwksKeys := ""
	if spec.JwksUri != "" {
		var err error
		jwksKeys, err = fetchJWKS(spec.JwksUri)
		if err != nil {
			utils.AviLog.Warnf("key: %s, msg: JWKS fetch failed for %s/%s: %v — will use cached keys if available",
				key, policy.Namespace, policy.Name, err)
		}
	}

	cksum := strconv.FormatUint(uint64(utils.Hash(spec.Issuer+spec.JwksUri+jwksKeys)), 10)
	profileType := "CLIENT_AUTH"
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)

	profile := avimodels.JWTServerProfile{
		Name:           proto.String(name),
		TenantRef:      proto.String(tenantRef),
		Issuer:         proto.String(spec.Issuer),
		JwtProfileType: proto.String(profileType),
	}
	if jwksKeys != "" {
		profile.JwksKeys = proto.String(jwksKeys)
	}

	client := avicache.SharedAVIClients(tenant).AviClient[0]
	cache := sharedJWTCache()

	if existing, ok := cache.getJWTProfile(tenant, name); ok && existing.CloudConfigCksum == cksum {
		utils.AviLog.Debugf("key: %s, msg: JWTServerProfile %s unchanged, skipping PUT", key, name)
		return "/api/jwtserverprofile?name=" + name, nil
	}

	if existing, ok := cache.getJWTProfile(tenant, name); ok {
		// UPDATE
		uri := "/api/jwtserverprofile/" + existing.UUID
		var resp interface{}
		if err := lib.AviPut(client, uri, profile, &resp); err != nil {
			return "", fmt.Errorf("JWTServerProfile PUT %s: %w", name, err)
		}
		existing.CloudConfigCksum = cksum
		cache.setJWTProfile(tenant, existing)
		utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s updated", key, name)
	} else {
		// CREATE — first check if it already exists in Avi (e.g. from a previous AKO instance).
		var existing struct {
			Count   int `json:"count"`
			Results []struct {
				UUID string `json:"uuid"`
			} `json:"results"`
		}
		_ = lib.AviGet(client, "/api/jwtserverprofile?name="+name, &existing)
		if existing.Count > 0 {
			uuid := existing.Results[0].UUID
			uri := "/api/jwtserverprofile/" + uuid
			var resp interface{}
			if err := lib.AviPut(client, uri, profile, &resp); err != nil {
				return "", fmt.Errorf("JWTServerProfile PUT existing %s: %w", name, err)
			}
			cache.setJWTProfile(tenant, &JWTProfileCacheEntry{Name: name, UUID: uuid, CloudConfigCksum: cksum})
			utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s (pre-existing) updated", key, name)
		} else {
			var resp struct {
				UUID string `json:"uuid"`
			}
			if err := lib.AviPost(client, "/api/jwtserverprofile", profile, &resp); err != nil {
				return "", fmt.Errorf("JWTServerProfile POST %s: %w", name, err)
			}
			cache.setJWTProfile(tenant, &JWTProfileCacheEntry{Name: name, UUID: resp.UUID, CloudConfigCksum: cksum})
			utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s created (uuid=%s)", key, name, resp.UUID)
		}
	}

	return "/api/jwtserverprofile?name=" + name, nil
}

// DeleteJWTServerProfile deletes the AKO-managed JWTServerProfile for the policy.
// Called when AIGatewayAuthPolicy is deleted.
func DeleteJWTServerProfile(key string, policy *AIGatewayAuthPolicy) {
	name := jwtProfileName(policy)
	tenant := lib.GetTenant()
	cache := sharedJWTCache()
	entry, ok := cache.getJWTProfile(tenant, name)
	if !ok {
		return
	}
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	if err := lib.AviDelete(client, "/api/jwtserverprofile/"+entry.UUID); err != nil {
		utils.AviLog.Warnf("key: %s, msg: JWTServerProfile DELETE %s failed: %v", key, name, err)
		return
	}
	cache.deleteJWTProfile(tenant, name)
	utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s deleted", key, name)
}

// ─── SSOPolicy lifecycle ──────────────────────────────────────────────────────

// EnsureSSOPolicy creates or updates the Avi SSO Policy (type SSO_TYPE_JWT)
// for the given AIGatewayAuthPolicy. jwtProfileRef is the path returned by
// EnsureJWTServerProfile. Returns the SSO Policy name for use in SsoPolicyRef.
func EnsureSSOPolicy(key string, policy *AIGatewayAuthPolicy, jwtProfileRef string) (string, error) {
	name := derivedSSOPolicyName(policy)
	tenant := lib.GetTenant()
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)
	authProfileRef := fmt.Sprintf("/api/authprofile/?name=%s", jwtProfileName(policy)+"-auth")

	// Ensure the AuthProfile (JWT type) exists first.
	if err := ensureJWTAuthProfile(key, policy, jwtProfileRef); err != nil {
		return "", err
	}

	ssoType := "SSO_TYPE_JWT"
	ssoPolicy := avimodels.SSOPolicy{
		Name:      proto.String(name),
		TenantRef: proto.String(tenantRef),
		Type:      proto.String(ssoType),
		AuthenticationPolicy: &avimodels.AuthenticationPolicy{
			DefaultAuthProfileRef: proto.String(authProfileRef),
		},
	}

	cksum := strconv.FormatUint(uint64(utils.Hash(name+jwtProfileRef+policy.Spec.JWT.Issuer)), 10)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	cache := sharedJWTCache()

	if existing, ok := cache.getSSOPolicy(tenant, name); ok && existing.CloudConfigCksum == cksum {
		utils.AviLog.Debugf("key: %s, msg: SSOPolicy %s unchanged, skipping PUT", key, name)
		return name, nil
	}

	if existing, ok := cache.getSSOPolicy(tenant, name); ok {
		uri := "/api/ssopolicy/" + existing.UUID
		var resp interface{}
		if err := lib.AviPut(client, uri, ssoPolicy, &resp); err != nil {
			return "", fmt.Errorf("SSOPolicy PUT %s: %w", name, err)
		}
		existing.CloudConfigCksum = cksum
		cache.setSSOPolicy(tenant, existing)
		utils.AviLog.Infof("key: %s, msg: SSOPolicy %s updated", key, name)
	} else {
		var check struct {
			Count   int `json:"count"`
			Results []struct {
				UUID string `json:"uuid"`
			} `json:"results"`
		}
		_ = lib.AviGet(client, "/api/ssopolicy?name="+name, &check)
		if check.Count > 0 {
			uuid := check.Results[0].UUID
			uri := "/api/ssopolicy/" + uuid
			var resp interface{}
			if err := lib.AviPut(client, uri, ssoPolicy, &resp); err != nil {
				return "", fmt.Errorf("SSOPolicy PUT existing %s: %w", name, err)
			}
			cache.setSSOPolicy(tenant, &SSOPolicyCacheEntry{Name: name, UUID: uuid, CloudConfigCksum: cksum})
			utils.AviLog.Infof("key: %s, msg: SSOPolicy %s (pre-existing) updated", key, name)
		} else {
			var resp struct {
				UUID string `json:"uuid"`
			}
			if err := lib.AviPost(client, "/api/ssopolicy", ssoPolicy, &resp); err != nil {
				return "", fmt.Errorf("SSOPolicy POST %s: %w", name, err)
			}
			cache.setSSOPolicy(tenant, &SSOPolicyCacheEntry{Name: name, UUID: resp.UUID, CloudConfigCksum: cksum})
			utils.AviLog.Infof("key: %s, msg: SSOPolicy %s created (uuid=%s)", key, name, resp.UUID)
		}
	}
	return name, nil
}

// DeleteSSOPolicy deletes the AKO-managed SSO Policy and its backing AuthProfile.
func DeleteSSOPolicy(key string, policy *AIGatewayAuthPolicy) {
	name := derivedSSOPolicyName(policy)
	tenant := lib.GetTenant()
	cache := sharedJWTCache()
	if entry, ok := cache.getSSOPolicy(tenant, name); ok {
		client := avicache.SharedAVIClients(tenant).AviClient[0]
		if err := lib.AviDelete(client, "/api/ssopolicy/"+entry.UUID); err != nil {
			utils.AviLog.Warnf("key: %s, msg: SSOPolicy DELETE %s failed: %v", key, name, err)
		} else {
			cache.deleteSSOPolicy(tenant, name)
			utils.AviLog.Infof("key: %s, msg: SSOPolicy %s deleted", key, name)
		}
	}
	deleteJWTAuthProfile(key, policy)
}

// ─── AuthProfile (JWT) lifecycle ─────────────────────────────────────────────

// ensureJWTAuthProfile creates/updates an Avi AuthProfile of type AUTH_PROFILE_JWT
// that wraps the JWTServerProfile. The SSO Policy references this AuthProfile.
func ensureJWTAuthProfile(key string, policy *AIGatewayAuthPolicy, jwtProfileRef string) error {
	name := jwtProfileName(policy) + "-auth"
	tenant := lib.GetTenant()
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)

	// Avi AuthProfile with JWT validation config.
	profile := map[string]interface{}{
		"name":          name,
		"tenant_ref":    tenantRef,
		"type":          "AUTH_PROFILE_JWT",
		"jwt_profile_ref": jwtProfileRef,
	}

	client := avicache.SharedAVIClients(tenant).AviClient[0]

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/authprofile?name="+name, &check)
	if check.Count > 0 {
		uuid := check.Results[0].UUID
		var resp interface{}
		if err := lib.AviPut(client, "/api/authprofile/"+uuid, profile, &resp); err != nil {
			return fmt.Errorf("AuthProfile PUT %s: %w", name, err)
		}
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/authprofile", profile, &resp); err != nil {
			return fmt.Errorf("AuthProfile POST %s: %w", name, err)
		}
	}
	utils.AviLog.Debugf("key: %s, msg: AuthProfile %s ensured", key, name)
	return nil
}

func deleteJWTAuthProfile(key string, policy *AIGatewayAuthPolicy) {
	name := jwtProfileName(policy) + "-auth"
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	var check struct {
		Count   int `json:"count"`
		Results []struct{ UUID string `json:"uuid"` } `json:"results"`
	}
	_ = lib.AviGet(client, "/api/authprofile?name="+name, &check)
	if check.Count > 0 {
		_ = lib.AviDelete(client, "/api/authprofile/"+check.Results[0].UUID)
		utils.AviLog.Infof("key: %s, msg: AuthProfile %s deleted", key, name)
	}
}
