/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"fmt"
	"regexp"
	"strings"
)

// Which MariaDB databases are an app's.
//
// MariaDB keeps no owner of a database, and every app's database is on one
// shared server. So what is an app's is decided by name, and the same names
// decide what the app's user may touch:
//
//   - the provisioned database (DatabaseName), and
//   - every database whose name begins with the provisioned name and an
//     underscore: demo_crm_reports is demo_crm's, demo_crm2 is not.
//
// The second half is what an app allowed to create databases of its own
// (allowDynamicDatabaseCreation) is granted, and all it is granted; it has
// to give its databases such names, and can create no other.
//
// One exception keeps the rule exact. Provisioned names are built from a
// tenant's and an app's with hyphens turned into underscores, so one app's
// provisioned database can lie under another's prefix: demo_crm_extra is the
// database of app crm-extra in tenant demo, and of any app of a tenant
// called demo-crm. A database under the prefix that another account holds
// rights on is that account's and not this app's (mariadbOwnedSQL), and
// provisioning refuses to create the overlap where a grant would span it
// (MariaDBSetupScript).
//
// Everything below derives from these functions: the grants provisioning
// issues, the one query export, restore, purge and the deletion of a tenant
// enumerate by, and nothing else spells the rule out.

// mariadbName is what a provisioned database's and a user's name may be
// made of. Both are derived by the platform; every script is refused for a
// name that is not, because the names are written into SQL.
var mariadbName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// mariadbGrantEscaper makes a name literal in a grant's database pattern
// and in LIKE: there "_" stands for any one character and "%" for any run,
// so an unescaped demo_crm also names demoXcrm.
var mariadbGrantEscaper = strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)

// MariaDBDatabasePattern is the provisioned database's name as a grant
// pattern that matches that database and no other.
func MariaDBDatabasePattern(database string) string {
	return mariadbGrantEscaper.Replace(database)
}

// MariaDBOwnedPattern is the pattern of the databases that are an app's
// besides the provisioned one: the provisioned name, an underscore, and
// whatever follows. It is a grant's database pattern and a LIKE pattern at
// once -- MariaDB matches both the same way -- which is what lets the
// grant and the enumeration be one rule.
func MariaDBOwnedPattern(database string) string {
	return MariaDBDatabasePattern(database) + `\_%`
}

// MariaDBOwns applies the name half of the rule to one database name, as
// the server does (byte for byte: database names are case-sensitive on the
// platform's server). The other half -- that no other account holds rights
// on the database -- only the server can answer.
func MariaDBOwns(provisioned, name string) bool {
	return name == provisioned || strings.HasPrefix(name, provisioned+"_")
}

// MariaDBGrants are the privileges an app's user holds, as the statements
// that grant them: everything on its provisioned database, and for an app
// that may create databases of its own, everything on the databases under
// its prefix. Nothing is granted on *.*: creating a database needs CREATE
// on that database's name, which the pattern gives, and no global
// privilege. ALL PRIVILEGES at this level does not include GRANT OPTION.
func MariaDBGrants(database, user string, dynamic bool) []string {
	grants := []string{
		fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'", MariaDBDatabasePattern(database), user),
	}
	if dynamic {
		grants = append(grants,
			fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'", MariaDBOwnedPattern(database), user))
	}
	return grants
}

// MariaDBRevokeAll takes away everything an app's user holds, at every
// level. Provisioning issues it when the user holds anything it is not to
// (mariadbStraySQL) and grants again: that is how a user provisioned with
// ALL PRIVILEGES ON *.* WITH GRANT OPTION, or with an unescaped database
// name, comes down to MariaDBGrants.
func MariaDBRevokeAll(user string) string {
	return fmt.Sprintf("REVOKE ALL PRIVILEGES, GRANT OPTION FROM '%s'@'%%'", user)
}

// mariadbHeldByAnother is the condition that some account other than the
// app's user holds database-level rights that reach the database named by
// the SQL expression name. mysql.db is where the server keeps those rights,
// one row per account and pattern.
func mariadbHeldByAnother(name, user string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM mysql.db g WHERE NOT (g.User = '%s' AND g.Host = '%%') AND BINARY %s LIKE BINARY g.Db)`,
		user, name)
}

// mariadbOwnedSQL is the one question "which databases besides the
// provisioned one are this app's": those under its prefix that no other
// account holds rights on. It ends without a semicolon so that it can be
// counted, limited and ordered by whoever asks.
func mariadbOwnedSQL(database, user string) string {
	return fmt.Sprintf(`SELECT s.schema_name FROM information_schema.schemata s
 WHERE BINARY s.schema_name LIKE BINARY '%s'
   AND NOT %s`, MariaDBOwnedPattern(database), mariadbHeldByAnother("s.schema_name", user))
}

// mariadbOthersSQL is its complement under the prefix: the databases there
// that are another account's, which every act leaves alone and names.
func mariadbOthersSQL(database, user string) string {
	return fmt.Sprintf(`SELECT s.schema_name FROM information_schema.schemata s
 WHERE BINARY s.schema_name LIKE BINARY '%s'
   AND %s`, MariaDBOwnedPattern(database), mariadbHeldByAnother("s.schema_name", user))
}

// mariadbStraySQL is the condition that the user holds something other than
// MariaDBGrants: a global privilege or the right to grant, a database-level
// grant on any other pattern or with the right to grant, or a grant on a
// single table. Privileges on routines are left out: the server gives a
// user those on each routine it creates.
func mariadbStraySQL(database, user string, dynamic bool) string {
	held := "'" + MariaDBDatabasePattern(database) + "'"
	if dynamic {
		held += ", '" + MariaDBOwnedPattern(database) + "'"
	}
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM information_schema.user_privileges
              WHERE grantee = '''%[1]s''@''%%''' AND (privilege_type <> 'USAGE' OR is_grantable = 'YES'))
     OR EXISTS (SELECT 1 FROM mysql.db WHERE User = '%[1]s' AND Host = '%%'
              AND (Grant_priv = 'Y' OR BINARY Db NOT IN (%[2]s)))
     OR EXISTS (SELECT 1 FROM mysql.tables_priv WHERE User = '%[1]s' AND Host = '%%')`, user, held)
}

// mariadbProvisioningLock serialises the provisioning of MariaDB apps on
// the server: the refusals below read what other accounts hold and then
// grant, and two apps whose names overlap must not both pass.
const mariadbProvisioningLock = "gentian-mariadb-provisioning"

// mariadbClient is how a script reaches the server: as its admin, by the
// admin Secret's environment (MariaDBAdminEnv).
const mariadbClient = `mariadb --host="${MYSQL_HOST}" --port="${MYSQL_TCP_PORT}" --user="${MYSQL_ADMIN_USER}"`

// mariadbRefusal is the script that runs in place of one that would have
// written an unsafe name into SQL, or "" when both names are safe.
func mariadbRefusal(database, user string) string {
	for what, name := range map[string]string{"database": database, "user": user} {
		if !mariadbName.MatchString(name) {
			return fmt.Sprintf("set -eu\necho %s >&2\nexit 1\n",
				shellSingleQuote(fmt.Sprintf("ERROR: invalid MariaDB %s name %q", what, name)))
		}
	}
	return ""
}

// MariaDBSetupScript makes an app's database and user and gives the user
// MariaDBGrants, exactly: it can be run again at any time, and a run leaves
// the user holding those grants and nothing else.
//
// The password is read from DB_PASS and handed to the server as hex, so no
// character of it is ever part of a statement.
//
// It refuses, before it creates anything, when the names overlap another
// app's -- which a grant cannot express and so must not be allowed to
// arise:
//
//   - when another account's rights already reach this app's database (it
//     lies under the prefix of an app that creates its own);
//   - when the app is to create its own and a database under its prefix is
//     another account's.
//
// Either fails the Job with the reason, and the app is not provisioned
// until the overlap is gone.
func MariaDBSetupScript(database, user string, dynamic bool) string {
	if refusal := mariadbRefusal(database, user); refusal != "" {
		return refusal
	}
	overlap := ""
	if dynamic {
		overlap = fmt.Sprintf(`
  SET other = (%[1]s LIMIT 1);
  IF other IS NOT NULL THEN
    SET why = CONCAT('refused: %[2]s is to create databases named %[2]s_..., and ', other, ' is one another account holds rights on');
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = why;
  END IF;`, mariadbOthersSQL(database, user), database)
	}
	return fmt.Sprintf(`set -euo pipefail
if [ -z "${DB_PASS:-}" ]; then
  echo "ERROR: DB_PASS must not be empty" >&2; exit 1
fi
PASS_HEX="$(printf '%%s' "${DB_PASS}" | od -An -v -tx1 | tr -d ' \n')"
{
printf "SET @pass = CONVERT(UNHEX('%%s') USING utf8mb4);\n" "${PASS_HEX}"
cat <<'SQL'
DELIMITER //
BEGIN NOT ATOMIC
  DECLARE other VARCHAR(255) DEFAULT NULL;
  DECLARE why VARCHAR(512);
  IF GET_LOCK('%[9]s', 120) IS NOT TRUE THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'the provisioning lock could not be taken';
  END IF;
  SET other = (SELECT CONCAT(g.User, '@', g.Host, ' on ', g.Db) FROM mysql.db g
                WHERE NOT (g.User = '%[2]s' AND g.Host = '%%') AND BINARY '%[1]s' LIKE BINARY g.Db LIMIT 1);
  IF other IS NOT NULL THEN
    SET why = CONCAT('refused: database %[1]s is within the rights of another account (', other, ')');
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = why;
  END IF;%[3]s
  CREATE DATABASE IF NOT EXISTS `+"`%[1]s`"+`;
  EXECUTE IMMEDIATE CONCAT('CREATE USER IF NOT EXISTS ''%[2]s''@''%%'' IDENTIFIED BY ', QUOTE(@pass));
  EXECUTE IMMEDIATE CONCAT('ALTER USER ''%[2]s''@''%%'' IDENTIFIED BY ', QUOTE(@pass));
  IF %[4]s THEN
    %[5]s;
  END IF;
  %[6]s;
  DO RELEASE_LOCK('%[9]s');
END//
SQL
} | %[7]s
echo "database %[1]s and user %[2]s ensured; the user holds: %[8]s"
`, database, user, overlap, mariadbStraySQL(database, user, dynamic), MariaDBRevokeAll(user),
		strings.Join(MariaDBGrants(database, user, dynamic), ";\n  "), mariadbClient,
		mariadbHoldsText(database, dynamic), mariadbProvisioningLock)
}

func mariadbHoldsText(database string, dynamic bool) string {
	if dynamic {
		return "all privileges on " + database + " and on databases named " + database + "_..., none on the server"
	}
	return "all privileges on " + database + ", none on the server"
}

// mariadbDestroyScript drops every database that is the app's
// (mariadbOwnedSQL), the provisioned one, and the user.
//
// The databases an app made are named by the app, so the server builds each
// DROP itself from the name it holds, quoted: no name passes through the
// shell. The user goes last, so that a run that stops half-way leaves the
// account whose rights say whose the remaining databases are. REVOKE is not
// issued first: DROP USER takes the user's privileges with it.
//
// It ends in success only when the server, asked again, has none of them
// left. A database under the prefix that another account holds rights on is
// not the app's; it is left and named.
func mariadbDestroyScript(database, user string) string {
	if refusal := mariadbRefusal(database, user); refusal != "" {
		return refusal
	}
	return fmt.Sprintf(`set -euo pipefail
MARIADB=(%[4]s)
"${MARIADB[@]}" <<'SQL'
DELIMITER //
BEGIN NOT ATOMIC
  DECLARE nm VARCHAR(255);
  owned: LOOP
    SET nm = (%[3]s LIMIT 1);
    IF nm IS NULL THEN LEAVE owned; END IF;
    EXECUTE IMMEDIATE CONCAT('DROP DATABASE `+"`', REPLACE(nm, '`', '``'), '`"+`');
  END LOOP;
  DROP DATABASE IF EXISTS `+"`%[1]s`"+`;
  DROP USER IF EXISTS '%[2]s'@'%%';
END//
SQL
left="$("${MARIADB[@]}" -N -s <<'SQL'
SELECT COUNT(*) FROM information_schema.schemata s
 WHERE BINARY s.schema_name = '%[1]s' OR s.schema_name IN (%[3]s);
SQL
)"
if [ "${left}" != "0" ]; then
  echo "ERROR: ${left} database(s) of %[1]s are still there" >&2; exit 1
fi
"${MARIADB[@]}" -N -s --raw <<'SQL'
SELECT CONCAT('NOTE: ', o.schema_name, ' is named like a database of %[1]s and another account holds rights on it; it was left as it is')
  FROM (%[5]s) o;
SQL
echo "deleted database %[1]s, every database named %[1]s_... that was its own, and user %[2]s"
`, database, user, mariadbOwnedSQL(database, user), mariadbClient, mariadbOthersSQL(database, user))
}
