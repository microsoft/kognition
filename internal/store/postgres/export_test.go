// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import "github.com/jackc/pgx/v5/pgxpool"

// SharedTestPool exposes the embedded database to the external test package.
func SharedTestPool() *pgxpool.Pool { return testPool }
