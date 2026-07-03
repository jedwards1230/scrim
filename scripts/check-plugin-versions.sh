#!/usr/bin/env bash
#
# check-plugin-versions.sh
#
# Validates that plugin versions are properly bumped and consistent across
# plugin.json and marketplace.json when plugin files are changed.
#
# Usage: ./scripts/check-plugin-versions.sh <base-ref>
#   base-ref: Git reference to compare against (e.g., origin/main)
#
# Exit codes:
#   0 - All checks passed
#   1 - Version validation failed
#   2 - Script error (missing dependencies, invalid arguments)

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

MARKETPLACE_FILE=".claude-plugin/marketplace.json"
ERRORS_FOUND=0

# Arrays to track results for summary
declare -a PLUGIN_RESULTS=()
declare -a ERROR_MESSAGES=()
METADATA_RESULT=""

# Check dependencies
check_dependencies() {
    if ! command -v jq &> /dev/null; then
        echo -e "${RED}ERROR: jq is required but not installed${NC}"
        exit 2
    fi
}

# Print error message (to stdout to maintain order)
error() {
    echo -e "${RED}ERROR: $1${NC}"
    ERROR_MESSAGES+=("$1")
}

# Print success message
success() {
    echo -e "${GREEN}$1${NC}"
}

# Print info message
info() {
    echo -e "${YELLOW}$1${NC}"
}

# Get version from plugin.json at a specific git ref
# Arguments: $1 = plugin name, $2 = git ref
get_plugin_version() {
    local plugin_name="$1"
    local ref="$2"
    local plugin_json="plugins/${plugin_name}/.claude-plugin/plugin.json"

    git show "${ref}:${plugin_json}" 2>/dev/null | jq -r '.version // empty' 2>/dev/null || echo ""
}

# Get version from marketplace.json entry at a specific git ref
# Arguments: $1 = plugin name, $2 = git ref
get_marketplace_version() {
    local plugin_name="$1"
    local ref="$2"

    git show "${ref}:${MARKETPLACE_FILE}" 2>/dev/null | \
        jq -r --arg name "$plugin_name" '.plugins[] | select(.name == $name) | .version // empty' 2>/dev/null || echo ""
}

# Get metadata.version from marketplace.json at a specific git ref
# Arguments: $1 = git ref
get_metadata_version() {
    local ref="$1"

    git show "${ref}:${MARKETPLACE_FILE}" 2>/dev/null | jq -r '.metadata.version // empty' 2>/dev/null || echo ""
}

# Get list of changed plugins from git diff
# Arguments: $1 = base ref
get_changed_plugins() {
    local base_ref="$1"

    git diff --name-only "${base_ref}"...HEAD -- 'plugins/' 2>/dev/null | \
        grep -E '^plugins/[^/]+/' | \
        sed 's|^plugins/\([^/]*\)/.*|\1|' | \
        sort -u || true
}

# Check if marketplace.json itself was changed
# Arguments: $1 = base ref
marketplace_changed() {
    local base_ref="$1"

    git diff --name-only "${base_ref}"...HEAD -- "${MARKETPLACE_FILE}" 2>/dev/null | grep -q . && return 0 || return 1
}

# Check if a plugin is new (doesn't exist in base ref)
# Arguments: $1 = plugin name, $2 = base ref
is_new_plugin() {
    local plugin_name="$1"
    local base_ref="$2"
    local base_version

    base_version=$(get_plugin_version "$plugin_name" "$base_ref")
    [[ -z "$base_version" ]]
}

# Validate a single plugin
# Arguments: $1 = plugin name, $2 = base ref
validate_plugin() {
    local plugin_name="$1"
    local base_ref="$2"
    local has_errors=0
    local plugin_status="pass"
    local plugin_details=""

    echo ""
    echo "Checking plugin: ${plugin_name}"
    echo "----------------------------------------"

    # Get versions
    local base_plugin_version
    local head_plugin_version
    local base_marketplace_version
    local head_marketplace_version

    base_plugin_version=$(get_plugin_version "$plugin_name" "$base_ref")
    head_plugin_version=$(get_plugin_version "$plugin_name" "HEAD")
    base_marketplace_version=$(get_marketplace_version "$plugin_name" "$base_ref")
    head_marketplace_version=$(get_marketplace_version "$plugin_name" "HEAD")

    # Check if plugin.json exists in HEAD
    if [[ -z "$head_plugin_version" ]]; then
        error "plugin.json not found or missing version for ${plugin_name}"
        ((ERRORS_FOUND++))
        PLUGIN_RESULTS+=("${plugin_name}|fail|plugin.json not found or missing version")
        return 1
    fi

    # Check if marketplace entry exists in HEAD
    if [[ -z "$head_marketplace_version" ]]; then
        error "Missing marketplace.json entry for ${plugin_name}"
        error "  Add an entry to .claude-plugin/marketplace.json plugins array"
        ((ERRORS_FOUND++))
        PLUGIN_RESULTS+=("${plugin_name}|fail|Missing marketplace.json entry")
        return 1
    fi

    # Check version consistency between plugin.json and marketplace.json
    if [[ "$head_plugin_version" != "$head_marketplace_version" ]]; then
        error "Version mismatch for ${plugin_name}"
        error "  plugin.json version:     ${head_plugin_version}"
        error "  marketplace.json version: ${head_marketplace_version}"
        error "  These versions must be identical"
        ((ERRORS_FOUND++))
        has_errors=1
        plugin_status="fail"
        plugin_details="Version mismatch: plugin.json=${head_plugin_version}, marketplace=${head_marketplace_version}"
    fi

    # For new plugins, only check consistency (already done above)
    if is_new_plugin "$plugin_name" "$base_ref"; then
        info "New plugin detected - skipping bump check"
        if [[ $has_errors -eq 0 ]]; then
            success "Version consistency check passed: ${head_plugin_version}"
            PLUGIN_RESULTS+=("${plugin_name}|pass|New plugin, version ${head_plugin_version}")
        else
            PLUGIN_RESULTS+=("${plugin_name}|${plugin_status}|${plugin_details}")
        fi
        return $has_errors
    fi

    # Check that plugin.json version was bumped
    if [[ "$base_plugin_version" == "$head_plugin_version" ]]; then
        error "plugin.json version not bumped for ${plugin_name}"
        error "  Base version:    ${base_plugin_version}"
        error "  Current version: ${head_plugin_version}"
        error "  Please increment the version when changing plugin files"
        ((ERRORS_FOUND++))
        has_errors=1
        plugin_status="fail"
        plugin_details="${plugin_details:+${plugin_details}; }plugin.json not bumped (${base_plugin_version})"
    else
        success "plugin.json version bumped: ${base_plugin_version} -> ${head_plugin_version}"
    fi

    # Check that marketplace.json entry version was bumped
    if [[ "$base_marketplace_version" == "$head_marketplace_version" ]]; then
        error "marketplace.json entry version not bumped for ${plugin_name}"
        error "  Base version:    ${base_marketplace_version}"
        error "  Current version: ${head_marketplace_version}"
        error "  Please update the version in .claude-plugin/marketplace.json"
        ((ERRORS_FOUND++))
        has_errors=1
        plugin_status="fail"
        plugin_details="${plugin_details:+${plugin_details}; }marketplace entry not bumped (${base_marketplace_version})"
    else
        success "marketplace.json entry version bumped: ${base_marketplace_version} -> ${head_marketplace_version}"
    fi

    if [[ $has_errors -eq 0 ]]; then
        plugin_details="${base_plugin_version} → ${head_plugin_version}"
    fi
    PLUGIN_RESULTS+=("${plugin_name}|${plugin_status}|${plugin_details}")

    return $has_errors
}

# Check if metadata.version was bumped when plugins changed
# Arguments: $1 = base ref
check_metadata_version() {
    local base_ref="$1"

    echo ""
    echo "Checking marketplace metadata version"
    echo "----------------------------------------"

    local base_metadata_version
    local head_metadata_version

    base_metadata_version=$(get_metadata_version "$base_ref")
    head_metadata_version=$(get_metadata_version "HEAD")

    if [[ -z "$head_metadata_version" ]]; then
        error "metadata.version not found in marketplace.json"
        ((ERRORS_FOUND++))
        METADATA_RESULT="fail|metadata.version not found"
        return 1
    fi

    if [[ "$base_metadata_version" == "$head_metadata_version" ]]; then
        error "metadata.version not bumped in marketplace.json"
        error "  Base version:    ${base_metadata_version}"
        error "  Current version: ${head_metadata_version}"
        error "  Please bump metadata.version when plugins change"
        ((ERRORS_FOUND++))
        METADATA_RESULT="fail|Not bumped (${base_metadata_version})"
        return 1
    fi

    success "metadata.version bumped: ${base_metadata_version} -> ${head_metadata_version}"
    METADATA_RESULT="pass|${base_metadata_version} → ${head_metadata_version}"
    return 0
}

# Print fix instructions
print_fix_instructions() {
    echo ""
    echo "To fix version errors:"
    echo "  1. Bump the version in plugins/<name>/.claude-plugin/plugin.json"
    echo "  2. Update the matching entry in .claude-plugin/marketplace.json"
    echo "  3. Ensure both versions are identical"
    echo "  4. Bump metadata.version in .claude-plugin/marketplace.json"
}

# Write GitHub Actions job summary
write_summary() {
    # Only write summary if GITHUB_STEP_SUMMARY is set (running in GitHub Actions)
    if [[ -z "${GITHUB_STEP_SUMMARY:-}" ]]; then
        return
    fi

    {
        echo "## Plugin Version Check"
        echo ""

        if [[ ${#PLUGIN_RESULTS[@]} -eq 0 ]]; then
            echo "No plugin changes detected."
        else
            echo "### Plugin Results"
            echo ""
            echo "| Plugin | Status | Details |"
            echo "|--------|--------|---------|"
            for result in "${PLUGIN_RESULTS[@]}"; do
                IFS='|' read -r name status details <<< "$result"
                if [[ "$status" == "pass" ]]; then
                    echo "| \`${name}\` | :white_check_mark: Pass | ${details} |"
                else
                    echo "| \`${name}\` | :x: Fail | ${details} |"
                fi
            done
            echo ""

            if [[ -n "$METADATA_RESULT" ]]; then
                echo "### Marketplace Metadata"
                echo ""
                IFS='|' read -r status details <<< "$METADATA_RESULT"
                if [[ "$status" == "pass" ]]; then
                    echo ":white_check_mark: \`.claude-plugin/marketplace.json\` **metadata.version**: ${details}"
                else
                    echo ":x: \`.claude-plugin/marketplace.json\` **metadata.version**: ${details}"
                fi
                echo ""
            fi
        fi

        if [[ $ERRORS_FOUND -gt 0 ]]; then
            echo "### :x: Check Failed"
            echo ""
            echo "Found **${ERRORS_FOUND} error(s)**. Please fix the version issues above."
            echo ""
            echo "<details>"
            echo "<summary>How to fix</summary>"
            echo ""
            echo "1. Bump the version in \`plugins/<name>/.claude-plugin/plugin.json\`"
            echo "2. Update the matching entry in \`.claude-plugin/marketplace.json\`"
            echo "3. Ensure both versions are identical"
            echo "4. Bump \`metadata.version\` in \`.claude-plugin/marketplace.json\`"
            echo ""
            echo "**Version bump conventions for metadata.version:**"
            echo "- Plugin added/removed: Major bump (1.2.3 → 2.0.0)"
            echo "- Core metadata changes: Minor bump (1.2.3 → 1.3.0)"
            echo "- Plugin version changes: Patch bump (1.2.3 → 1.2.4)"
            echo ""
            echo "</details>"
        else
            echo "### :white_check_mark: All Checks Passed"
        fi
    } >> "$GITHUB_STEP_SUMMARY"
}

# Main function
main() {
    if [[ $# -ne 1 ]]; then
        echo "Usage: $0 <base-ref>" >&2
        echo "  base-ref: Git reference to compare against (e.g., origin/main)" >&2
        exit 2
    fi

    local base_ref="$1"

    check_dependencies

    echo "Plugin Version Check"
    echo "===================="
    echo "Comparing HEAD against ${base_ref}"

    # Get list of changed plugins
    local changed_plugins
    changed_plugins=$(get_changed_plugins "$base_ref")

    if [[ -z "$changed_plugins" ]]; then
        # Check if only marketplace.json changed (plugin might have been removed)
        if marketplace_changed "$base_ref"; then
            info "No plugin files changed, but marketplace.json was modified"
            check_metadata_version "$base_ref" || true
        else
            success "No plugin changes detected - nothing to validate"
        fi
    else
        echo ""
        echo "Changed plugins: $(echo "$changed_plugins" | tr '\n' ' ')"

        # Validate each changed plugin
        for plugin in $changed_plugins; do
            validate_plugin "$plugin" "$base_ref" || true
        done

        # Check metadata version bump
        check_metadata_version "$base_ref" || true
    fi

    # Final summary
    echo ""
    echo "=========================================="
    if [[ $ERRORS_FOUND -gt 0 ]]; then
        error "Version check failed with ${ERRORS_FOUND} error(s)"
        print_fix_instructions
        write_summary
        exit 1
    else
        success "All version checks passed!"
        write_summary
        exit 0
    fi
}

main "$@"