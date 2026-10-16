$ErrorActionPreference = 'Stop'
$taskRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$taskBaseline = Join-Path $taskRoot '.task/baseline'
New-Item -ItemType Directory -Force $taskBaseline | Out-Null
if (-not (Test-Path (Join-Path $taskBaseline 'go.mod'))) {
    $taskArchive = Join-Path $taskRoot '.task/benchmark-baseline.tar'
    git -C $taskRoot archive --format=tar -o $taskArchive 4907a7a -- go.mod go.sum internal
    if ($LASTEXITCODE -ne 0) { throw 'Baseline revision 4907a7a is required.' }
    tar -xf $taskArchive -C $taskBaseline
    if ($LASTEXITCODE -ne 0) { throw 'Baseline extraction failed.' }
}
Copy-Item -LiteralPath (Join-Path $taskRoot 'internal/downloader/performance_matrix_test.go') -Destination (Join-Path $taskBaseline 'internal/downloader/performance_matrix_test.go')
$taskBefore = Join-Path $taskRoot 'docs/optimization/matrix-baseline.txt'
$taskAfter = Join-Path $taskRoot 'docs/optimization/matrix-after.txt'
Set-Content -LiteralPath $taskBefore -Value ''
Set-Content -LiteralPath $taskAfter -Value ''
foreach ($taskCase in @('tiny','small','medium','large','latency','connection-limited','bandwidth-limited','slow-tail','mixed')) {
    $taskPattern = if ($taskCase -eq 'mixed') { '^BenchmarkMixedQueue$' } else { '^BenchmarkTransferMatrix/' + [regex]::Escape($taskCase) + '$' }
    $taskIterations = if ($taskCase -in @('tiny','small')) { '20x' } else { '1x' }
    foreach ($taskRound in 1..5) {
        foreach ($taskVariant in @('baseline','after')) {
            $taskDir = if ($taskVariant -eq 'baseline') { Join-Path $taskRoot '.task/baseline' } else { $taskRoot }
            $taskOutput = if ($taskVariant -eq 'baseline') { $taskBefore } else { $taskAfter }
            Push-Location $taskDir
            try {
                $taskLines = & go test ./internal/downloader -run '^$' -bench $taskPattern -benchmem ("-benchtime=" + $taskIterations) -count=1 -timeout=3m
                if ($LASTEXITCODE -ne 0) { $taskLines | Write-Output; throw "Benchmark failed: $taskCase / $taskVariant" }
                $taskLines | Add-Content -LiteralPath $taskOutput
                $taskLines | Select-String '^Benchmark' | ForEach-Object { Write-Output "$taskVariant : $($_.Line)" }
            } finally { Pop-Location }
        }
    }
}
Write-Output 'Paired matrix: PASS (nine scenarios, five alternating rounds each)'
