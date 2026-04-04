package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

// VKCallInfo contains the result of creating a VK call.
type VKCallInfo struct {
	JoinLink     string
	SignalingURL string
	ICEServers   []ICEServerRaw
	CallID       string
}

// ICEServerRaw is the raw ICE server config from VK API.
type ICEServerRaw struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// VKAPIClient handles VK authentication and call creation.
type VKAPIClient struct {
	client    *http.Client
	accessToken string
}

// NewVKAPIClient creates a client authenticated with the given cookies.
// cookiesRaw should be Netscape/curl format cookies for vk.com.
func NewVKAPIClient(cookiesRaw string) (*VKAPIClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	vkURL, _ := url.Parse("https://vk.com")
	var cookies []*http.Cookie
	for _, line := range strings.Split(cookiesRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:   fields[5],
			Value:  fields[6],
			Domain: fields[0],
			Path:   fields[2],
		})
	}
	jar.SetCookies(vkURL, cookies)

	client := &http.Client{Jar: jar}
	return &VKAPIClient{client: client}, nil
}

// Authenticate retrieves the access token from VK using cookies.
func (c *VKAPIClient) Authenticate() error {
	// Try to get access token via login.vk.com implicit flow.
	authURL := "https://login.vk.com/?act=web_token&app_id=6287487&version=1"
	resp, err := c.client.Get(authURL)
	if err != nil {
		return fmt.Errorf("auth request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}

	var authResp struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &authResp); err != nil {
		return fmt.Errorf("parse auth response: %w", err)
	}
	if authResp.Data.AccessToken == "" {
		return fmt.Errorf("no access token in response (check cookies)")
	}
	c.accessToken = authResp.Data.AccessToken
	return nil
}

// StartCall creates a new VK call and returns call info.
func (c *VKAPIClient) StartCall() (*VKCallInfo, error) {
	if c.accessToken == "" {
		return nil, fmt.Errorf("not authenticated")
	}

	params := url.Values{
		"access_token": {c.accessToken},
		"v":            {"5.199"},
	}

	resp, err := c.client.PostForm("https://api.vk.com/method/calls.start", params)
	if err != nil {
		return nil, fmt.Errorf("calls.start: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var apiResp struct {
		Response struct {
			JoinLink string `json:"join_link"`
			CallID   string `json:"call_id"`
			Settings struct {
				ICEServers []ICEServerRaw `json:"ice_servers"`
			} `json:"settings"`
		} `json:"response"`
		Error struct {
			ErrorCode int    `json:"error_code"`
			ErrorMsg  string `json:"error_msg"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if apiResp.Error.ErrorCode != 0 {
		return nil, fmt.Errorf("VK API error %d: %s", apiResp.Error.ErrorCode, apiResp.Error.ErrorMsg)
	}

	return &VKCallInfo{
		JoinLink:   apiResp.Response.JoinLink,
		CallID:     apiResp.Response.CallID,
		ICEServers: apiResp.Response.Settings.ICEServers,
	}, nil
}
