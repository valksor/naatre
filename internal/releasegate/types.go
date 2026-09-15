// Package releasegate validates and aggregates the complete, externally
// executed Naatre conformance matrix. It never executes or invents evidence.
package releasegate

type Input struct {
	Root              string
	FixturePath       string
	ProfileReports    []string
	IndependentReport string
	SecurityReport    string
	SourceRevision    string
}

type Inventory struct {
	Protocol    string           `json:"protocol"`
	Versions    Versions         `json:"versions"`
	Languages   []string         `json:"languages"`
	Matrix      []ExpectedMatrix `json:"matrix"`
	RunnerPaths []ExpectedRunner `json:"runnerPaths"`
}

type ExpectedMatrix struct {
	Kind           string        `json:"kind"`
	Implementation string        `json:"implementation"`
	Language       string        `json:"language"`
	Profile        string        `json:"profile"`
	EvidenceRole   string        `json:"evidenceRole"`
	EligiblePaths  []ProfilePath `json:"eligiblePaths"`
}

type ExpectedRunner struct {
	Kind     string `json:"kind"`
	Language string `json:"language"`
	Profile  string `json:"profile"`
}

type Report struct {
	Protocol       string         `json:"protocol"`
	Status         string         `json:"status"`
	SourceRevision string         `json:"sourceRevision"`
	Versions       Versions       `json:"versions"`
	Inputs         []Evidence     `json:"inputs"`
	Matrix         []MatrixResult `json:"matrix"`
	RunnerPaths    []RunnerPath   `json:"runnerPaths"`
	SharedVectors  []SharedVector `json:"sharedVectors"`
	Failures       []Failure      `json:"failures"`
}

type Versions struct {
	Gate             string `json:"gate"`
	Spec             string `json:"spec"`
	Fixtures         string `json:"fixtures"`
	Profiles         string `json:"profiles"`
	RunnerProtocol   string `json:"runnerProtocol"`
	ReportProtocol   string `json:"reportProtocol"`
	Canonicalization string `json:"canonicalization"`
	SchemaRevision   string `json:"schemaRevision"`
}

type Evidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Endpoint struct {
	Kind     string `json:"kind"`
	Language string `json:"language"`
}

type ExecutionPath struct {
	Source      Endpoint `json:"source"`
	Destination Endpoint `json:"destination"`
}

type OptionalCapability struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type MatrixResult struct {
	Kind                 string               `json:"kind"`
	Implementation       string               `json:"implementation"`
	Language             string               `json:"language"`
	Profile              string               `json:"profile"`
	EvidenceRole         string               `json:"evidenceRole"`
	Path                 ExecutionPath        `json:"path"`
	Report               Evidence             `json:"report"`
	Status               string               `json:"status"`
	OptionalCapabilities []OptionalCapability `json:"optionalCapabilities"`
}

type RunnerPath struct {
	Kind     string   `json:"kind"`
	Language string   `json:"language"`
	Profile  string   `json:"profile"`
	Report   Evidence `json:"report"`
	Status   string   `json:"status"`
}

type SharedVector struct {
	Name      string   `json:"name"`
	SHA256    string   `json:"sha256"`
	Languages []string `json:"languages"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type gateFixture struct {
	Profile                     string   `json:"profile"`
	GateVersion                 string   `json:"gateVersion"`
	SpecVersion                 string   `json:"specVersion"`
	FixtureVersion              string   `json:"fixtureVersion"`
	ProfileRegistryVersion      string   `json:"profileRegistryVersion"`
	RunnerProtocol              string   `json:"runnerProtocol"`
	ReportProtocol              string   `json:"reportProtocol"`
	AggregateProtocol           string   `json:"aggregateProtocol"`
	CompatibilityMatrix         string   `json:"compatibilityMatrix"`
	ProfileRegistry             string   `json:"profileRegistry"`
	ProfileReportSchema         string   `json:"profileReportSchema"`
	ProfileReportSchemaSHA256   string   `json:"profileReportSchemaSHA256"`
	AggregateReportSchema       string   `json:"aggregateReportSchema"`
	AggregateReportSchemaSHA256 string   `json:"aggregateReportSchemaSHA256"`
	ReleaseManifestSchema       string   `json:"releaseManifestSchema"`
	ReleaseManifestSchemaSHA256 string   `json:"releaseManifestSchemaSHA256"`
	SharedVectorArtifacts       []string `json:"sharedVectorArtifacts"`
	RequiredRunnerPaths         []string `json:"requiredRunnerPaths"`
	ExecutionOwnerIssue         int      `json:"executionOwnerIssue"`
	PublicationOwnerIssue       int      `json:"publicationOwnerIssue"`
	SecurityGate                struct {
		Implementation string `json:"implementation"`
		Language       string `json:"language"`
		Profile        string `json:"profile"`
	} `json:"securityGate"`
	IndependentRunner struct {
		Name         string `json:"name"`
		Language     string `json:"language"`
		Source       string `json:"source"`
		SourceSHA256 string `json:"sourceSHA256"`
		Profile      string `json:"profile"`
	} `json:"independentRunner"`
}

type suiteManifest struct {
	SpecVersion            string `json:"specVersion"`
	FixtureVersion         string `json:"fixtureVersion"`
	ProfileRegistryVersion string `json:"profileRegistryVersion"`
	RunnerProtocol         string `json:"runnerProtocol"`
	ReportProtocol         string `json:"reportProtocol"`
	Languages              []string
	Files                  []struct {
		Path    string `json:"path"`
		Profile string `json:"profile"`
		SHA256  string `json:"sha256"`
	} `json:"files"`
}

type profileRegistry struct {
	RegistryVersion    string              `json:"registryVersion"`
	SpecVersion        string              `json:"specVersion"`
	FixtureVersion     string              `json:"fixtureVersion"`
	RunnerProtocol     string              `json:"runnerProtocol"`
	ReportProtocol     string              `json:"reportProtocol"`
	ReportSchemaSHA256 string              `json:"reportSchemaSHA256"`
	Profiles           []profileDefinition `json:"profiles"`
}

type profileDefinition struct {
	ID                   string               `json:"id"`
	EvidenceRole         string               `json:"evidenceRole"`
	RequiredClauses      []string             `json:"requiredClauses"`
	RequiredFixtures     []requiredFixture    `json:"requiredFixtures"`
	EligiblePaths        []ProfilePath        `json:"eligiblePaths"`
	OptionalCapabilities []optionalDefinition `json:"optionalCapabilities"`
}

type requiredFixture struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ProfilePath struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type optionalDefinition struct {
	ID       string `json:"id"`
	Fixture  string `json:"fixture"`
	Required bool   `json:"required"`
}

type compatibilityMatrix struct {
	Profile                string                        `json:"profile"`
	ProfileRegistryVersion string                        `json:"profileRegistryVersion"`
	SpecVersion            string                        `json:"specVersion"`
	FixtureVersion         string                        `json:"fixtureVersion"`
	ReleaseStatus          string                        `json:"releaseStatus"`
	ExecutionOwnerIssue    int                           `json:"executionOwnerIssue"`
	PublicationOwnerIssue  int                           `json:"publicationOwnerIssue"`
	Implementations        []compatibilityImplementation `json:"implementations"`
	IntegrationPaths       []compatibilityPath           `json:"integrationPaths"`
}

type compatibilityImplementation struct {
	Implementation  string   `json:"implementation"`
	Language        string   `json:"language"`
	Status          string   `json:"status"`
	PlannedProfiles []string `json:"plannedProfiles"`
}

type compatibilityPath struct {
	Source struct {
		Implementation string `json:"implementation"`
		Language       string `json:"language"`
		Kind           string `json:"kind"`
	} `json:"source"`
	Destination struct {
		Implementation string `json:"implementation"`
		Language       string `json:"language"`
		Kind           string `json:"kind"`
	} `json:"destination"`
	Profile string `json:"profile"`
	Status  string `json:"status"`
}

type profileReport struct {
	Protocol string `json:"protocol"`
	Run      struct {
		ID        string          `json:"id"`
		Operator  string          `json:"operator"`
		Command   []string        `json:"command"`
		Artifacts []namedArtifact `json:"artifacts"`
	} `json:"run"`
	Versions struct {
		Spec             string `json:"spec"`
		Fixtures         string `json:"fixtures"`
		Profiles         string `json:"profiles"`
		RunnerProtocol   string `json:"runnerProtocol"`
		Canonicalization string `json:"canonicalization"`
		SchemaRevision   string `json:"schemaRevision"`
	} `json:"versions"`
	Revisions      revisionSet `json:"revisions"`
	Implementation struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		Language       string `json:"language"`
		RuntimeVersion string `json:"runtimeVersion"`
		SpecVersion    string `json:"specVersion"`
		FixtureVersion string `json:"fixtureVersion"`
		ProfileVersion string `json:"profileVersion"`
		SchemaRevision string `json:"schemaRevision"`
	} `json:"implementation"`
	Environment struct {
		OS                       string   `json:"os"`
		Architecture             string   `json:"architecture"`
		FeatureFlags             []string `json:"featureFlags"`
		WireTransports           []string `json:"wireTransports"`
		StreamTransports         []string `json:"streamTransports"`
		ScalarPrecision          []string `json:"scalarPrecision"`
		CancellationCapabilities []string `json:"cancellationCapabilities"`
	} `json:"environment"`
	Path    ExecutionPath `json:"path"`
	Claims  []claim       `json:"claims"`
	Results []result      `json:"results"`
}

type namedArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type revisionSet struct {
	Specification revision           `json:"specification"`
	Schema        revision           `json:"schema"`
	Canonical     revision           `json:"canonicalization"`
	Fixtures      revision           `json:"fixtures"`
	Generator     applicableRevision `json:"generator"`
	Runtime       applicableRevision `json:"runtime"`
	SDK           applicableRevision `json:"sdk"`
	Transport     applicableRevision `json:"transport"`
}

type revision struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type applicableRevision struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Reason  string `json:"reason"`
}

type claim struct {
	Profile      string   `json:"profile"`
	Capabilities []string `json:"capabilities"`
}

type result struct {
	Profile         string            `json:"profile"`
	EvidenceRole    string            `json:"evidenceRole"`
	Status          string            `json:"status"`
	Capabilities    []string          `json:"capabilities"`
	Capability      string            `json:"capability"`
	ExecutedClauses []string          `json:"executedClauses"`
	Evidence        []profileEvidence `json:"evidence"`
	Diagnostics     []diagnostic      `json:"diagnostics"`
	Skip            *skip             `json:"skip"`
}

type profileEvidence struct {
	Fixture string `json:"fixture"`
	SHA256  string `json:"sha256"`
}

type diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type skip struct {
	Fixture string `json:"fixture"`
	Reason  string `json:"reason"`
}

type runnerReport struct {
	Protocol       string `json:"protocol"`
	FixtureVersion string `json:"fixtureVersion"`
	Runner         struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Language string `json:"language"`
		Platform string `json:"platform"`
	} `json:"runner"`
	Path    ExecutionPath `json:"path"`
	Results []struct {
		Profile  string            `json:"profile"`
		Status   string            `json:"status"`
		Evidence []profileEvidence `json:"evidence"`
	} `json:"results"`
}
