#!/bin/bash

# GenericAgent macOS Desktop App Installation Script
# Installs GenericAgent as a native macOS desktop application
#
# Usage: bash scripts/install-macos-app.sh [--auto]
#
# Options:
#   --auto    Non-interactive mode, skip prompts and install directly

if [ -z "${BASH_VERSION}" ]; then
    if command -v bash >/dev/null 2>&1; then
        exec bash -- "${0}" "$@"
    else
        echo "Error: This script requires bash."
        exit 1
    fi
fi

set -eo pipefail

# ============================================
# Colors
# ============================================
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
log_info()    { echo -e "${BLUE}ℹ️  $1${NC}"; }
log_success() { echo -e "${GREEN}✅ $1${NC}"; }
log_warning() { echo -e "${YELLOW}⚠️  $1${NC}"; }
log_error()   { echo -e "${RED}❌ $1${NC}"; }

# ============================================
# Parse arguments
# ============================================
AUTO_MODE=false
for arg in "$@"; do
    case "$arg" in --auto) AUTO_MODE=true ;; esac
done

# ============================================
# Configuration
# ============================================
APP_NAME="GenericAgent"
APP_PATH="/Applications/${APP_NAME}.app"

# Icon: bundled alongside this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ICON_PATH="${PROJECT_ROOT}/assets/images/logo.jpg"

# ============================================
# Pre-flight checks
# ============================================
echo -e "${CYAN}"
echo "╔═══════════════════════════════════════════════════════════╗"
echo "║   GenericAgent — macOS Desktop App Installer             ║"
echo "╚═══════════════════════════════════════════════════════════╝"
echo -e "${NC}"

if [[ "$(uname)" != "Darwin" ]]; then
    log_error "This script only supports macOS."
    exit 1
fi

# Check Python and pip
if ! command -v python3 &>/dev/null; then
    log_error "python3 is not installed."
    exit 1
fi

# Check if already installed
APP_ALREADY_INSTALLED=false
if [ -d "$APP_PATH" ]; then
    APP_ALREADY_INSTALLED=true
    log_warning "GenericAgent.app already exists in /Applications."
fi

# Interactive prompt
if [ "$AUTO_MODE" = false ]; then
    echo ""
    echo "This will install a desktop app that launches GenericAgent"
    echo "from Spotlight (Cmd+Space), Launchpad, or the Applications folder."
    echo ""
    if [ "$APP_ALREADY_INSTALLED" = true ]; then
        read -p "Reinstall GenericAgent.app? (y/N) " -n 1 -r
    else
        read -p "Continue? (Y/n) " -n 1 -r
    fi
    echo
    if [ "$APP_ALREADY_INSTALLED" = true ]; then
        [[ ! $REPLY =~ ^[Yy]$ ]] && { echo "Aborted."; exit 0; }
    else
        [[ $REPLY =~ ^[Nn]$ ]] && { echo "Aborted."; exit 0; }
    fi
fi

# Remove existing app
[ -d "$APP_PATH" ] && rm -rf "$APP_PATH"

# ============================================
# Build the app
# ============================================
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

log_info "Building GenericAgent.app..."

# Create AppleScript — launches launch.pyw in project directory
# The script prompts for project path on first run, or uses a default
cat > "${TMP_DIR}/GenericAgent.applescript" << 'APPLESCRIPT'
property defaultPath : ""

on run
    set projectPath to defaultPath
    
    if projectPath is "" then
        -- Ask user for GenericAgent project folder
        set projectPath to choose folder with prompt "Select your GenericAgent project folder:"
    end if
    
    set projectPathStr to POSIX path of projectPath
    set launchScript to projectPathStr & "launch.pyw"
    
    tell application "Terminal"
        activate
        do script "cd " & quoted form of projectPathStr & " && python3 launch.pyw"
    end tell
end run
APPLESCRIPT

# Compile to .app
osacompile -o "${TMP_DIR}/${APP_NAME}.app" "${TMP_DIR}/GenericAgent.applescript" 2>/dev/null

# ============================================
# Install icon
# ============================================
log_info "Applying GenericAgent icon..."

if [ -f "$ICON_PATH" ]; then
    ICONSET_DIR="${TMP_DIR}/ga-icon.iconset"
    mkdir -p "$ICONSET_DIR"
    
    sips -z 16 16   "$ICON_PATH" --out "${ICONSET_DIR}/icon_16x16.png"       >/dev/null 2>&1
    sips -z 32 32   "$ICON_PATH" --out "${ICONSET_DIR}/icon_16x16@2x.png"     >/dev/null 2>&1
    sips -z 32 32   "$ICON_PATH" --out "${ICONSET_DIR}/icon_32x32.png"        >/dev/null 2>&1
    sips -z 64 64   "$ICON_PATH" --out "${ICONSET_DIR}/icon_32x32@2x.png"     >/dev/null 2>&1
    sips -z 128 128 "$ICON_PATH" --out "${ICONSET_DIR}/icon_128x128.png"      >/dev/null 2>&1
    sips -z 256 256 "$ICON_PATH" --out "${ICONSET_DIR}/icon_128x128@2x.png"   >/dev/null 2>&1
    sips -z 256 256 "$ICON_PATH" --out "${ICONSET_DIR}/icon_256x256.png"      >/dev/null 2>&1
    sips -z 512 512 "$ICON_PATH" --out "${ICONSET_DIR}/icon_256x256@2x.png"   >/dev/null 2>&1
    sips -z 512 512 "$ICON_PATH" --out "${ICONSET_DIR}/icon_512x512.png"      >/dev/null 2>&1
    cp "$ICON_PATH" "${ICONSET_DIR}/icon_512x512@2x.png"
    
    iconutil -c icns "$ICONSET_DIR" -o "${TMP_DIR}/ga-icon.icns" 2>/dev/null
    cp "${TMP_DIR}/ga-icon.icns" "${TMP_DIR}/${APP_NAME}.app/Contents/Resources/applet.icns"
    log_success "Icon applied from assets/images/logo.jpg"
else
    log_warning "Logo not found at ${ICON_PATH}, using default"
fi

# ============================================
# Install to /Applications
# ============================================
cp -R "${TMP_DIR}/${APP_NAME}.app" "/Applications/"
log_success "Installed to: ${APP_PATH}"

# ============================================
# Post-install: refresh icon cache
# ============================================
rm ~/Library/Application\ Support/Dock/*.db 2>/dev/null || true
killall Dock 2>/dev/null || true

# ============================================
# Summary
# ============================================
echo ""
echo -e "${CYAN}╔═══════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║${NC}  ✨  GenericAgent Desktop App installed successfully!        ${CYAN}║${NC}"
echo -e "${CYAN}╚═══════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "${BLUE}Launch methods:${NC}"
echo "  • Spotlight:  Cmd + Space → type 'GenericAgent' → Enter"
echo "  • Launchpad:  Find the 'GenericAgent' icon"
echo "  • Finder:     Open /Applications/GenericAgent.app"
echo ""
echo -e "${BLUE}First run:${NC}"
echo "  The app will ask you to select your GenericAgent project folder."
echo "  It then runs 'python3 launch.pyw' from that directory."
echo ""
echo -e "${BLUE}Set default project folder:${NC}"
echo "  Edit /Applications/GenericAgent.app and change the defaultPath property"
echo "  in Contents/Resources/Scripts/main.scpt (or re-run this installer)."
echo ""
echo -e "${BLUE}Uninstall:${NC}"
echo "  rm -rf '/Applications/GenericAgent.app'"
echo ""
