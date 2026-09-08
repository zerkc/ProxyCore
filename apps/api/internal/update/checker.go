package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRepository = "zerkc/ProxyCore"
	DefaultBaseURL    = "https://api.github.com"
	DefaultTTL        = 6 * time.Hour
	DefaultTimeout    = 5 * time.Second
)

const (
	StatusCurrent         = "current"
	StatusUpdateAvailable = "update_available"
	StatusStale           = "stale"
	StatusUnavailable     = "unavailable"
	StatusDisabled        = "disabled"
)

type Release struct {
	Version     string     `json:"version"`
	Tag         string     `json:"tag"`
	URL         string     `json:"url"`
	PublishedAt *time.Time `json:"publishedAt"`
}

type Result struct {
	Status           string     `json:"status"`
	CurrentVersion   string     `json:"currentVersion"`
	Latest           *Release   `json:"latest"`
	UpdateAvailable  bool       `json:"updateAvailable"`
	CheckedAt        *time.Time `json:"checkedAt"`
	UpdateInProgress bool       `json:"updateInProgress"`
	TargetVersion    string     `json:"targetVersion,omitempty"`
}

type CheckerOptions struct {
	CurrentVersion string
	Enabled        bool
	TTL            time.Duration
	Timeout        time.Duration
	Client         *http.Client
	BaseURL        string
	Repository     string
	Now            func() time.Time
}

type Checker struct {
	currentVersion string
	enabled        bool
	ttl            time.Duration
	timeout        time.Duration
	client         *http.Client
	baseURL        string
	repository     string
	now            func() time.Time

	mu       sync.Mutex
	cached   *Result
	expires  time.Time
	inFlight chan struct{}
}

type githubRelease struct {
	TagName     string     `json:"tag_name"`
	HTMLURL     string     `json:"html_url"`
	PublishedAt *time.Time `json:"published_at"`
}

type semanticVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease []string
}

func NewChecker(options CheckerOptions) *Checker {
	ttl := options.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	repository := strings.TrimSpace(options.Repository)
	if !validRepository(repository) {
		repository = DefaultRepository
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Checker{
		currentVersion: normalizeVersion(options.CurrentVersion),
		enabled:        options.Enabled,
		ttl:            ttl,
		timeout:        timeout,
		client:         client,
		baseURL:        baseURL,
		repository:     repository,
		now:            now,
	}
}

func (c *Checker) Check(ctx context.Context) Result {
	if !c.enabled {
		return Result{
			Status:         StatusDisabled,
			CurrentVersion: c.currentVersion,
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := c.now().UTC()
	c.mu.Lock()
	if c.cached != nil && now.Before(c.expires) {
		result := cloneResult(*c.cached)
		c.mu.Unlock()
		return result
	}
	if c.inFlight != nil {
		done := c.inFlight
		cached := cloneResultPtr(c.cached)
		c.mu.Unlock()

		select {
		case <-done:
			return c.Check(ctx)
		case <-ctx.Done():
			return fallbackResult(c.currentVersion, cached)
		}
	}

	done := make(chan struct{})
	c.inFlight = done
	cached := cloneResultPtr(c.cached)
	c.mu.Unlock()

	latest, err := c.fetchLatestRelease(ctx)
	now = c.now().UTC()

	var result Result
	if err == nil {
		result, err = c.resultFor(latest, now)
	}

	c.mu.Lock()
	if err == nil {
		c.cached = cloneResultPtr(&result)
		c.expires = now.Add(c.ttl)
	} else {
		result = fallbackResult(c.currentVersion, cached)
		c.cached = cloneResultPtr(&result)
		c.expires = now.Add(c.failureTTL())
	}
	c.inFlight = nil
	close(done)
	c.mu.Unlock()

	return result
}

func (c *Checker) fetchLatestRelease(ctx context.Context) (Release, error) {
	endpoint, err := c.latestReleaseURL()
	if err != nil {
		return Release{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	userAgentVersion := c.currentVersion
	if userAgentVersion == "" {
		userAgentVersion = "unknown"
	}
	request.Header.Set("User-Agent", "ProxyCore/"+userAgentVersion)

	response, err := c.client.Do(request)
	if err != nil {
		return Release{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github returned %s", response.Status)
	}

	var payload githubRelease
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("decode github release: %w", err)
	}
	tag := strings.TrimSpace(payload.TagName)
	normalized := normalizeVersion(tag)
	parsed, err := parseSemVer(normalized)
	if err != nil {
		return Release{}, fmt.Errorf("invalid github release tag: %w", err)
	}
	if len(parsed.prerelease) > 0 {
		return Release{}, fmt.Errorf("github release is a prerelease")
	}
	if tag == "" || strings.TrimSpace(payload.HTMLURL) == "" {
		return Release{}, fmt.Errorf("github release is missing tag or URL")
	}

	return Release{
		Version:     normalized,
		Tag:         tag,
		URL:         strings.TrimSpace(payload.HTMLURL),
		PublishedAt: payload.PublishedAt,
	}, nil
}

func (c *Checker) resultFor(latest Release, checkedAt time.Time) (Result, error) {
	current, err := parseSemVer(c.currentVersion)
	if err != nil {
		return Result{}, fmt.Errorf("invalid current version: %w", err)
	}
	latestVersion, err := parseSemVer(latest.Version)
	if err != nil {
		return Result{}, err
	}
	if len(latestVersion.prerelease) > 0 {
		return Result{}, fmt.Errorf("latest release is a prerelease")
	}

	updateAvailable := compareSemVer(latestVersion, current) > 0
	status := StatusCurrent
	if updateAvailable {
		status = StatusUpdateAvailable
	}
	checkedAt = checkedAt.UTC()
	return Result{
		Status:          status,
		CurrentVersion:  c.currentVersion,
		Latest:          &latest,
		UpdateAvailable: updateAvailable,
		CheckedAt:       &checkedAt,
	}, nil
}

func (c *Checker) latestReleaseURL() (string, error) {
	parts := strings.SplitN(c.repository, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid repository %q", c.repository)
	}
	joined, err := url.JoinPath(c.baseURL, "repos", parts[0], parts[1], "releases", "latest")
	if err != nil {
		return "", fmt.Errorf("build github URL: %w", err)
	}
	return joined, nil
}

func (c *Checker) failureTTL() time.Duration {
	if c.ttl < 5*time.Minute {
		return c.ttl
	}
	return 5 * time.Minute
}

func fallbackResult(currentVersion string, cached *Result) Result {
	if cached != nil {
		result := cloneResult(*cached)
		if result.Latest != nil && result.CheckedAt != nil {
			result.Status = StatusStale
		}
		return result
	}
	return Result{
		Status:         StatusUnavailable,
		CurrentVersion: currentVersion,
	}
}

func cloneResultPtr(result *Result) *Result {
	if result == nil {
		return nil
	}
	cloned := cloneResult(*result)
	return &cloned
}

func cloneResult(result Result) Result {
	cloned := result
	if result.Latest != nil {
		latest := *result.Latest
		cloned.Latest = &latest
	}
	if result.CheckedAt != nil {
		checkedAt := *result.CheckedAt
		cloned.CheckedAt = &checkedAt
	}
	return cloned
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	return len(parts) == 2 &&
		strings.TrimSpace(parts[0]) != "" &&
		strings.TrimSpace(parts[1]) != "" &&
		!strings.ContainsAny(repository, " \t\r\n")
}

func normalizeVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "v") {
		return raw[1:]
	}
	return raw
}

func parseSemVer(raw string) (semanticVersion, error) {
	raw = normalizeVersion(raw)
	if raw == "" {
		return semanticVersion{}, fmt.Errorf("version is empty")
	}

	withoutBuild := raw
	if buildParts := strings.SplitN(raw, "+", 2); len(buildParts) == 2 {
		if !validIdentifierList(buildParts[1]) {
			return semanticVersion{}, fmt.Errorf("invalid build metadata")
		}
		withoutBuild = buildParts[0]
	}

	parts := strings.SplitN(withoutBuild, "-", 2)
	coreParts := strings.Split(parts[0], ".")
	if len(coreParts) != 3 {
		return semanticVersion{}, fmt.Errorf("version must contain major, minor, and patch")
	}
	major, err := parseCoreNumber(coreParts[0])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid major: %w", err)
	}
	minor, err := parseCoreNumber(coreParts[1])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid minor: %w", err)
	}
	patch, err := parseCoreNumber(coreParts[2])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid patch: %w", err)
	}

	version := semanticVersion{major: major, minor: minor, patch: patch}
	if len(parts) == 2 {
		if !validIdentifierList(parts[1]) {
			return semanticVersion{}, fmt.Errorf("invalid prerelease")
		}
		version.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range version.prerelease {
			if isNumericIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0' {
				return semanticVersion{}, fmt.Errorf("numeric prerelease has a leading zero")
			}
		}
	}
	return version, nil
}

func parseCoreNumber(raw string) (uint64, error) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, fmt.Errorf("invalid numeric identifier")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func validIdentifierList(raw string) bool {
	if raw == "" {
		return false
	}
	for _, identifier := range strings.Split(raw, ".") {
		if identifier == "" {
			return false
		}
		for _, character := range identifier {
			if !((character >= '0' && character <= '9') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= 'a' && character <= 'z') ||
				character == '-') {
				return false
			}
		}
	}
	return true
}

func isNumericIdentifier(identifier string) bool {
	if identifier == "" {
		return false
	}
	for _, character := range identifier {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareSemVer(left, right semanticVersion) int {
	if left.major != right.major {
		return compareUint(left.major, right.major)
	}
	if left.minor != right.minor {
		return compareUint(left.minor, right.minor)
	}
	if left.patch != right.patch {
		return compareUint(left.patch, right.patch)
	}
	if len(left.prerelease) == 0 && len(right.prerelease) > 0 {
		return 1
	}
	if len(left.prerelease) > 0 && len(right.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		leftIdentifier := left.prerelease[index]
		rightIdentifier := right.prerelease[index]
		if leftIdentifier == rightIdentifier {
			continue
		}
		leftNumeric := isNumericIdentifier(leftIdentifier)
		rightNumeric := isNumericIdentifier(rightIdentifier)
		if leftNumeric && rightNumeric {
			return compareNumericStrings(leftIdentifier, rightIdentifier)
		}
		if leftNumeric != rightNumeric {
			if leftNumeric {
				return -1
			}
			return 1
		}
		if leftIdentifier < rightIdentifier {
			return -1
		}
		return 1
	}
	return compareInt(len(left.prerelease), len(right.prerelease))
}

func compareNumericStrings(left, right string) int {
	left = strings.TrimLeft(left, "0")
	right = strings.TrimLeft(right, "0")
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	if len(left) != len(right) {
		return compareInt(len(left), len(right))
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareUint(left, right uint64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
