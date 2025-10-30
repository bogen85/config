function run {
    param(
        [Parameter(Mandatory = $true, Position = 0)]
        [string]$Executable,

        [Parameter(ValueFromRemainingArguments = $true)]
        [string[]]$Args
    )

    #if (-not $IsWindows) {
    #    throw "run: this helper is Windows-only."
    #}

    # env-based verbose
    $runVerbose = ($env:RUN_VERBOSE -eq '1')

    if ($runVerbose) {
        Write-Host "[run] requested exe: $Executable"
        if ($Args) {
            Write-Host "[run] requested args: $($Args -join ' | ')"
        } else {
            Write-Host "[run] requested args: (none)"
        }
    }

    # 1. make unique temp dir
    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ("run-" + [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
    if ($runVerbose) { Write-Host "[run] created temp dir: $tempDir" }

    # 2. resolve the executable
    if (Test-Path $Executable) {
        $exePath = (Resolve-Path $Executable).Path
        if ($runVerbose) { Write-Host "[run] executable is a path: $exePath" }
    } else {
        $cmd = Get-Command $Executable -ErrorAction SilentlyContinue
        if (-not $cmd) {
            throw "run: executable '$Executable' not found."
        }
        $exePath = $cmd.Source
        if ($runVerbose) { Write-Host "[run] executable found in PATH: $exePath" }
    }

    # 2a. on Windows, we really want an .exe (so we don't copy e.g. .ps1)
    #if (-not $exePath.EndsWith('.exe', 'InvariantCultureIgnoreCase')) {
    #    throw "run: expected a Windows .exe, got '$exePath'"
    #}

    # 3. copy to temp to avoid locking the original
    $tempExe = Join-Path $tempDir ([IO.Path]::GetFileName($exePath))
    Copy-Item -LiteralPath $exePath -Destination $tempExe -Force
    if ($runVerbose) { Write-Host "[run] copied to: $tempExe" }

    # 4. run from temp dir
    Push-Location $tempDir
    try {
        if ($runVerbose) {
            Write-Host "[run] running: $tempExe $($Args -join ' ')"
        }

        & $tempExe @Args
        $exit = $LASTEXITCODE

        if ($runVerbose) {
            Write-Host "[run] process exit code: $exit"
        }
    }
    finally {
        Pop-Location

        if ($runVerbose) {
            Write-Host "[run] cleaning up: $tempDir"
        }
        # even if Windows still has the exe open for a millisecond, this usually succeeds;
        # if not, we just ignore it
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    return $exit
}
