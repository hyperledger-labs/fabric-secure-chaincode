#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

# Note: This file is adapted from hyperledger/fabric scripts/functions.sh (ec81f3e74)

function filterExcludedAndGeneratedFiles {
    local excluded_files
    excluded_files=(
        '\.block$'
        '^\.build/'
        '^build/'
        '(^|/)ci\.properties$'
        '(^|/)\.git/'
        '\.gen\.go$'
        '(^|/)go.mod$'
        '(^|/)go.sum$'
        '(^|/)Gopkg\.lock$'
        '\.html$'
        '\.json$'
        '\.key$'
        '(^|/)LICENSE$'
        '\.md$'
        '\.pb\.go$'
        '\.pem$'
        '\.png$'
        '\.pptx$'
        '\.rst$'
        '_sk$'
        '\.tx$'
        '\.txt$'
        '^NOTICE$'
        '(^|/)testdata\/'
        '(^|/)vendor\/'
        '(^|/)Pipfile$'
        '(^|/)Pipfile\.lock$'
        '(^|/)tox\.ini$'
        'integration/test-network/fabric-samples/'
    )

    local filter
    filter=$(local IFS='|' ; echo "${excluded_files[*]}")

    files=($@)
    for f in "${files[@]}"; do
        file=$(echo "$f" | grep -Ev "$filter" | sort -u)
        if [ -n "$file" ]; then
            head -n2 "$file" | grep -qE '// Code generated by' || echo "$file"
        fi
    done
}
