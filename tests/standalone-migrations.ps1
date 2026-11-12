# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ProjectName = 'kognition-standalone-parity',
    [switch]$VerifyMemory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = Split-Path -Parent $PSScriptRoot
$databaseName = 'migration_check_' + [Guid]::NewGuid().ToString('N')
$imageName = 'kognition-migration-verification:local'
$previousDatabaseURL = $env:DATABASE_URL
$previousBaseline = $env:KOGNITION_MIGRATION_BASELINE
$previousMemoryDatabaseURL = $env:KOGNITION_TEST_MEMORY_DATABASE_URL
$databaseCreated = $false

function Invoke-TestSQL {
    param([string]$Database, [string]$SQL)
    $result = $SQL | & docker exec -i $script:postgresContainer psql -X -U kognition -d $Database -v ON_ERROR_STOP=1 -At
    if ($LASTEXITCODE -ne 0) { throw "SQL validation failed in $Database" }
    return $result
}

function Invoke-TestMigrator {
    param([string]$Baseline = '', [switch]$ExpectFailure)
    $env:KOGNITION_MIGRATION_BASELINE = $Baseline
    $output = & docker run --rm --network $script:networkName -e DATABASE_URL -e KOGNITION_MIGRATION_BASELINE $script:imageName 2>&1
    $exitCode = $LASTEXITCODE
    if ($ExpectFailure) {
        if ($exitCode -eq 0) { throw 'Migrator unexpectedly accepted the database' }
    } elseif ($exitCode -ne 0) {
        throw "Migrator failed: $output"
    }
}

Push-Location $repoRoot
try {
    $containers = @(& docker ps --filter "label=com.docker.compose.project=$ProjectName" --filter 'label=com.docker.compose.service=postgres' --format '{{.Names}}')
    if ($LASTEXITCODE -ne 0 -or $containers.Count -ne 1) {
        throw 'Expected exactly one running PostgreSQL container in the selected project'
    }
    $script:postgresContainer = $containers[0]
    $networks = & docker inspect --format '{{json .NetworkSettings.Networks}}' $postgresContainer | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect PostgreSQL network' }
    $networkNames = @($networks.PSObject.Properties.Name)
    if ($networkNames.Count -ne 1) { throw 'Expected exactly one PostgreSQL network' }
    $script:networkName = $networkNames[0]
    & docker build --target migrate -t $imageName .
    if ($LASTEXITCODE -ne 0) { throw 'Migrator image build failed' }

    $files = @(Get-ChildItem internal/store/postgres/migrations/*.sql | Sort-Object Name)
    $legacyFiles = @($files | Where-Object { $_.Name -le '030_usage_reporting.sql' })
    if ($legacyFiles.Count -eq 0 -or $legacyFiles[-1].Name -ne '030_usage_reporting.sql') {
        throw 'Legacy migration prefix is unavailable'
    }
    Invoke-TestSQL postgres "CREATE DATABASE $databaseName" | Out-Null
    $databaseCreated = $true
    $env:DATABASE_URL = "postgres://kognition:kognition@${postgresContainer}:5432/${databaseName}?sslmode=disable"

    Invoke-TestMigrator -Baseline '030_usage_reporting.sql' -ExpectFailure
    Invoke-TestMigrator
    $count = Invoke-TestSQL $databaseName 'SELECT count(*) FROM schema_migrations'
    if ([int]$count -ne $files.Count) { throw 'Fresh migration ledger is incomplete' }
    Invoke-TestMigrator

    Invoke-TestSQL postgres "DROP DATABASE $databaseName" | Out-Null
    $databaseCreated = $false
    Invoke-TestSQL postgres "CREATE DATABASE $databaseName" | Out-Null
    $databaseCreated = $true
    foreach ($file in $legacyFiles) {
        Invoke-TestSQL $databaseName (Get-Content $file.FullName -Raw) | Out-Null
    }
    Invoke-TestSQL $databaseName @'
INSERT INTO resources (kind, namespace, name, data)
VALUES ('Workspace', 'migration-check', 'retained', '{"proof":"retained"}');
INSERT INTO memory_vectors (volume_id, content, metadata)
VALUES ('11111111-1111-4111-8111-111111111111', 'retained-memory', '{"proof":"retained"}');
'@ | Out-Null
    Invoke-TestMigrator -ExpectFailure
    $ledger = Invoke-TestSQL $databaseName "SELECT to_regclass('schema_migrations') IS NULL"
    if ($ledger -ne 't') { throw 'Rejected baseline left a migration ledger behind' }
    Invoke-TestMigrator -Baseline '030_usage_reporting.sql'
    Invoke-TestMigrator
    $count = Invoke-TestSQL $databaseName 'SELECT count(*) FROM schema_migrations'
    if ([int]$count -ne $files.Count) { throw 'Legacy migration ledger is incomplete' }
    $retained = Invoke-TestSQL $databaseName @'
SELECT (SELECT count(*) FROM resources WHERE namespace = 'migration-check' AND data->>'proof' = 'retained')
     + (SELECT count(*) FROM memory_vectors WHERE content = 'retained-memory' AND metadata->>'proof' = 'retained');
'@
    if ([int]$retained -ne 2) { throw 'Migration did not retain resource and Memory data' }
    $coordinationTable = Invoke-TestSQL $databaseName "SELECT to_regclass('tool_resilience_state') IS NOT NULL"
    if ($coordinationTable -ne 't') { throw 'Pending migration was not applied' }
    if ($VerifyMemory) {
        $hostPort = & docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' $postgresContainer
        if ($LASTEXITCODE -ne 0 -or $hostPort -notmatch '^\d+$') { throw 'Cannot resolve local PostgreSQL port' }
        $env:KOGNITION_TEST_MEMORY_DATABASE_URL = "postgres://kognition:kognition@127.0.0.1:${hostPort}/${databaseName}?sslmode=disable"
        & go test .\internal\store\postgres -run '^(TestStandaloneMemoryMutationReplayAcrossRestart|TestPgvectorOpaqueVolumeHandleIsolation)$' -count=1 -timeout=3m -v
        if ($LASTEXITCODE -ne 0) { throw 'Standalone Memory verification failed' }
    }
    Write-Host "PASS: $($files.Count) real migrations; fresh install, explicit legacy baseline, repeat startup, retained resource and Memory rows."
} finally {
    try {
        if ($databaseCreated) {
            Invoke-TestSQL postgres "DROP DATABASE $databaseName" | Out-Null
        }
    } finally {
        $env:DATABASE_URL = $previousDatabaseURL
        $env:KOGNITION_MIGRATION_BASELINE = $previousBaseline
        $env:KOGNITION_TEST_MEMORY_DATABASE_URL = $previousMemoryDatabaseURL
        Pop-Location
    }
}