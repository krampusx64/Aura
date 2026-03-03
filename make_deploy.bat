@echo off
REM make_deploy.bat — Build AuraGo deployment artifacts (Windows host)
REM Requires: Go toolchain in PATH
setlocal enabledelayedexpansion
cd /d "%~dp0"

set DEPLOY_DIR=deploy
set RESOURCES=resources.dat

echo ━━━ AuraGo Deployment Builder ━━━
echo.

REM ── Clean ──────────────────────────────────────────────────────────────
if exist "%DEPLOY_DIR%" rd /s /q "%DEPLOY_DIR%"
mkdir "%DEPLOY_DIR%"

REM ── Step 1: Pack resources.dat ─────────────────────────────────────────
echo [1/3] Packing resources.dat ...

set TMPRES=%TEMP%\aurago_res_%RANDOM%
mkdir "%TMPRES%\agent_workspace" 2>nul
xcopy /E /I /Q agent_workspace\prompts  "%TMPRES%\agent_workspace\prompts"  >nul
xcopy /E /I /Q agent_workspace\skills   "%TMPRES%\agent_workspace\skills"   >nul
REM Remove credential files that must never be deployed
del /Q "%TMPRES%\agent_workspace\skills\client_secret.json" 2>nul
del /Q "%TMPRES%\agent_workspace\skills\client_secrets.json" 2>nul
del /Q "%TMPRES%\agent_workspace\skills\token.json" 2>nul
mkdir "%TMPRES%\agent_workspace\tools" 2>nul
mkdir "%TMPRES%\agent_workspace\workdir\attachments" 2>nul
mkdir "%TMPRES%\data\vectordb" 2>nul
mkdir "%TMPRES%\log" 2>nul

REM Copy config template (simple copy — manual sanitization recommended)
copy /Y config.yaml "%TMPRES%\config.yaml" >nul

REM Use tar (built into Windows 10+)
tar -czf "%DEPLOY_DIR%\%RESOURCES%" -C "%TMPRES%" .
echo     resources.dat created

rd /s /q "%TMPRES%"

REM ── Step 2: Cross-compile ──────────────────────────────────────────────
echo [2/3] Compiling binaries ...

for %%P in (linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64) do (
    for /f "tokens=1,2 delims=/" %%A in ("%%P") do (
        set CGO_ENABLED=0
        set GOOS=%%A
        set GOARCH=%%B

        if "%%A"=="linux" if "%%B"=="amd64" (
            REM Standard Linux release: put binaries in bin\ for GitHub updates
            if not exist "bin" mkdir "bin"
            
            set "OUT_AURAGO=bin\aurago_linux"
            echo     !OUT_AURAGO!
            go build -trimpath -ldflags="-s -w" -o "!OUT_AURAGO!" .\cmd\aurago\
            
            set "OUT_LIFEBOAT=bin\lifeboat_linux"
            echo     !OUT_LIFEBOAT!
            go build -trimpath -ldflags="-s -w" -o "!OUT_LIFEBOAT!" .\cmd\lifeboat\
        ) else (
            REM Other targets go to deploy\
            set "EXT="
            if "%%A"=="windows" set "EXT=.exe"
            set "OUT=%DEPLOY_DIR%\aurago_%%A_%%B!EXT!"
            echo     !OUT!
            go build -trimpath -ldflags="-s -w" -o "!OUT!" .\cmd\aurago\
        )
    )
)

REM ── Step 3: Copy install script ────────────────────────────────────────
echo [3/3] Copying install script ...
copy /Y install.sh "%DEPLOY_DIR%\install.sh" >nul 2>&1

echo.
echo ━━━ Done! Artifacts in %DEPLOY_DIR%\ ━━━
dir "%DEPLOY_DIR%"

REM ── Step 4: Auto Commit & Push ─────────────────────────────────────────
echo.
echo [4/4] Committing and pushing to GitHub ...
git add .
git diff-index --quiet HEAD || (
    git commit -m "build: auto-deploy artifacts and code updates" >nul
    git push origin main
    echo     Code pushed to GitHub successfully.
)
if errorlevel 0 echo     No changes to commit.
endlocal
