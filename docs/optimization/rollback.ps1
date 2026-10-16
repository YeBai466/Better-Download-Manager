param([string]$Branch = ("codex/rollback-download-pipeline-" + [DateTime]::UtcNow.ToString("yyyyMMddHHmmss")))
$ErrorActionPreference = 'Stop'
$baseline = '4907a7a'
$previous = git branch --show-current
if ($LASTEXITCODE -ne 0) { throw 'Run this script inside the project repository.' }
if (git status --porcelain) { throw 'Commit or stash local changes before switching revisions.' }
git switch -c $Branch $baseline
if ($LASTEXITCODE -ne 0) { throw 'The original checkout was retained; Git could not create the rollback branch.' }
$restored = git rev-parse HEAD
Write-Output "Restored source revision: $restored"
Write-Output "Optimized branch retained: $previous"
Write-Output 'Download data and existing build artifacts are retained. Rebuild this revision to run the baseline executable.'
