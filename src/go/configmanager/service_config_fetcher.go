// Copyright 2019 Google LLC
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

package configmanager

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/esp-v2/src/go/configmanager/flags"
	"github.com/GoogleCloudPlatform/esp-v2/src/go/metadata"
	"github.com/GoogleCloudPlatform/esp-v2/src/go/util"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"

	confpb "google.golang.org/genproto/googleapis/api/serviceconfig"
	smpb "google.golang.org/genproto/googleapis/api/servicemanagement/v1"
)

const (
	fetchConfigSuffix   = "/v1/services/$serviceName/configs/$configId?view=FULL"
	fetchRolloutsSuffix = "/v1/services/$serviceName/rollouts?filter=status=SUCCESS"
)

var (
	fetchConfigURL = func(serviceName, configID string) string {
		path := *flags.ServiceManagementURL + fetchConfigSuffix
		path = strings.Replace(path, "$serviceName", serviceName, 1)
		path = strings.Replace(path, "$configId", configID, 1)
		return path
	}
	fetchRolloutsURL = func(serviceName string) string {
		path := *flags.ServiceManagementURL + fetchRolloutsSuffix
		path = strings.Replace(path, "$serviceName", serviceName, 1)
		return path
	}
)

func newServiceConfigFetcherClient(timeout time.Duration) (*http.Client, error) {
	caCert, err := ioutil.ReadFile(*flags.RootCertsPath)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
		},
		Timeout: timeout,
	}, nil
}

func loadConfigFromRollouts(serviceName, curRolloutID, curConfigID string, mf *metadata.MetadataFetcher) (string, string, error) {
	var err error
	var listServiceRolloutsResponse *smpb.ListServiceRolloutsResponse
	listServiceRolloutsResponse, err = fetchRollouts(serviceName, mf)
	if err != nil {
		return "", "", fmt.Errorf("fail to get rollouts, %s", err)
	}

	if len(listServiceRolloutsResponse.Rollouts) == 0 {
		return "", "", fmt.Errorf("no active rollouts")
	}
	newRolloutID := listServiceRolloutsResponse.Rollouts[0].RolloutId
	if newRolloutID == curRolloutID {
		return curRolloutID, curConfigID, nil
	}
	glog.Infof("found new rollout id %v for service %v", newRolloutID, serviceName)
	glog.Infof("new rollout: %v", listServiceRolloutsResponse.Rollouts[0])

	trafficPercentStrategy := listServiceRolloutsResponse.Rollouts[0].GetTrafficPercentStrategy()
	trafficPercentMap := trafficPercentStrategy.GetPercentages()
	if len(trafficPercentMap) == 0 {
		return "", "", fmt.Errorf("no active rollouts")
	}
	var newConfigID string
	currentMaxPercent := 0.0
	// take config ID with max traffic percent as new config ID
	for k, v := range trafficPercentMap {
		if v > currentMaxPercent {
			newConfigID = k
			currentMaxPercent = v
		}
	}
	if newConfigID == curConfigID {
		glog.Infof("no new configuration to load for service %v, current configuration id %v", serviceName, curConfigID)
		return newRolloutID, curConfigID, nil
	}
	if !(math.Abs(100.0-currentMaxPercent) < 1e-9) {
		glog.Warningf("though traffic percentage of configuration %v is %v%%, set it to 100%%", newConfigID, currentMaxPercent)
	}
	glog.Infof("found new configuration id %v for service %v", curConfigID, serviceName)
	return newRolloutID, newConfigID, nil
}

func accessToken(mf *metadata.MetadataFetcher) (string, time.Duration, error) {
	if mf == nil && *flags.ServiceAccountKey == "" {
		return "", 0, fmt.Errorf("If --non_gcp is specified, --service_account_key has to be specified.")
	}
	if *flags.ServiceAccountKey != "" {
		return util.GenerateAccessTokenFromFile(*flags.ServiceAccountKey)
	}
	return mf.FetchAccessToken()
}

// TODO(jcwang) cleanup here. This function is redundant.
func fetchRollouts(serviceName string, mf *metadata.MetadataFetcher) (*smpb.ListServiceRolloutsResponse, error) {
	token, _, err := accessToken(mf)
	if err != nil {
		return nil, fmt.Errorf("fail to get access token: %v", err)
	}

	return callServiceManagementRollouts(fetchRolloutsURL(serviceName), token)
}

func fetchConfig(serviceName, configId string, mf *metadata.MetadataFetcher) (*confpb.Service, error) {
	token, _, err := accessToken(mf)
	if err != nil {
		return nil, fmt.Errorf("fail to get access token: %v", err)
	}
	return callServiceManagement(fetchConfigURL(serviceName, configId), token)
}

func readConfig(configPath string) (*confpb.Service, error) {
	config, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	return util.UnmarshalServiceConfig(bytes.NewReader(config))
}

var callServiceManagementRollouts = func(path, token string) (*smpb.ListServiceRolloutsResponse, error) {
	var err error
	var resp *http.Response
	if resp, err = callWithAccessToken(path, token); err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fail to read response body: %s", err)
	}
	defer resp.Body.Close()
	rolloutsResponse := new(smpb.ListServiceRolloutsResponse)
	if err := proto.Unmarshal(body, rolloutsResponse); err != nil {
		return nil, fmt.Errorf("fail to unmarshal ListServiceRolloutsResponse: %s", err)
	}
	return rolloutsResponse, nil
}

var callServiceManagement = func(path, token string) (*confpb.Service, error) {
	var err error
	var resp *http.Response
	if resp, err = callWithAccessToken(path, token); err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fail to read response body: %s", err)
	}
	defer resp.Body.Close()

	service := new(confpb.Service)
	if err := proto.Unmarshal(body, service); err != nil {
		return nil, fmt.Errorf("fail to unmarshal Service: %v", err)
	}
	return service, nil
}

var callWithAccessToken = func(path, token string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", path, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := serviceConfigFetcherClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("http call to %s returns not 200 OK: %v", path, resp.Status)
	}
	return resp, nil
}
