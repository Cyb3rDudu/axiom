@echo off
REM Startup script for AXIOM backend on Windows
REM This script runs database migrations before starting the FastAPI server

echo 🚀 Starting AXIOM Backend...

REM Run database migrations
echo 📊 Running database migrations...
python -m database.run_migrations

REM Check if migrations were successful
if errorlevel 1 (
    echo ❌ Database migrations failed!
    exit /b 1
) else (
    echo ✅ Database migrations completed successfully!
)

REM Start the FastAPI server
echo 🌐 Starting FastAPI server...
REM Convert LOG_LEVEL to lowercase for uvicorn
for /f "tokens=*" %%i in ('echo %LOG_LEVEL% ^| powershell -Command "$input = Read-Host; $input.ToLower()"') do set UVICORN_LOG_LEVEL=%%i
if "%UVICORN_LOG_LEVEL%"=="" set UVICORN_LOG_LEVEL=error

uvicorn main:app --host 0.0.0.0 --port 8000 --reload --log-level %UVICORN_LOG_LEVEL% 