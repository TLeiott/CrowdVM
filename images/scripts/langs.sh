#!/bin/bash
# Full profile: Boat's languages and runtimes (machines/tools) beyond Node, Bun,
# Python and Rust. System-wide in /usr/local and /opt; versions resolve to the
# newest release of the line Boat ships (recorded by tools.sh).
set -Eeuxo pipefail
export TAR_OPTIONS=--no-same-owner
apt_get() { apt-get -o DPkg::Lock::Timeout=600 -y "$@"; }
fetch() { curl -fsSL --retry 3 "$@"; }
# gh_tag <owner/repo> <tag regex>: newest release tag that matches.
gh_tag() {
	fetch "https://api.github.com/repos/$1/releases?per_page=100" |
		jq -r --arg re "$2" '[.[] | select(.prerelease == false and .draft == false) | .tag_name | select(test($re))][0]'
}
dl=$(mktemp -d)
keyrings=/etc/apt/keyrings
. /etc/os-release

# --- apt: PHP (ondrej PPA, as on Boat), R (CRAN) -------------------------------
add-apt-repository -y -n ppa:ondrej/php
fetch https://cloud.r-project.org/bin/linux/ubuntu/marutter_pubkey.asc | gpg --dearmor -o "$keyrings/cran.gpg"
echo "deb [signed-by=$keyrings/cran.gpg] https://cloud.r-project.org/bin/linux/ubuntu $VERSION_CODENAME-cran40/" \
	>/etc/apt/sources.list.d/cran.list
apt_get update
php=8.4
apt_get install --no-install-recommends \
	clang lld cmake ninja-build \
	openjdk-11-jdk-headless maven \
	erlang-nox erlang-dev elixir \
	dotnet-sdk-8.0 \
	r-base-core r-base-dev \
	php$php-cli php$php-common php$php-curl php$php-mbstring php$php-xml php$php-zip php$php-gd \
	php$php-intl php$php-sqlite3 php$php-mysql php$php-pgsql php$php-bcmath php$php-readline
# Composer (verified installer).
fetch -o "$dl/composer-setup.php" https://getcomposer.org/installer
[ "$(php -r "echo hash_file('sha384', '$dl/composer-setup.php');")" = "$(fetch https://composer.github.io/installer.sig)" ]
php "$dl/composer-setup.php" --quiet --install-dir=/usr/local/bin --filename=composer

# --- Go (latest stable) ----------------------------------------------------------
go=$(fetch 'https://go.dev/dl/?mode=json' | jq -r '.[0].version')
fetch "https://go.dev/dl/$go.linux-amd64.tar.gz" | tar -xz -C /usr/local
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/

# --- Deno ------------------------------------------------------------------------
fetch -o "$dl/deno.zip" https://github.com/denoland/deno/releases/latest/download/deno-x86_64-unknown-linux-gnu.zip
unzip -q -o "$dl/deno.zip" -d /usr/local/bin

# --- JVM: Gradle 8.x (9 needs JDK 17), Kotlin, Scala 3.3 LTS, sbt 1.x -----------
gradle=$(fetch https://services.gradle.org/versions/all | jq -r '[.[] | select(.snapshot == false and .nightly == false
	and .rcFor == "" and .milestoneFor == "" and .broken == false and (.version | test("^8\\.[0-9.]+$")))][0].version')
fetch -o "$dl/gradle.zip" "https://services.gradle.org/distributions/gradle-$gradle-bin.zip"
unzip -q "$dl/gradle.zip" -d /opt && mv "/opt/gradle-$gradle" /opt/gradle
kotlin=$(gh_tag JetBrains/kotlin '^v2\.[0-9]+\.[0-9]+$')
fetch -o "$dl/kotlin.zip" "https://github.com/JetBrains/kotlin/releases/download/$kotlin/kotlin-compiler-${kotlin#v}.zip"
unzip -q "$dl/kotlin.zip" -d /opt # -> /opt/kotlinc
scala=$(gh_tag scala/scala3 '^3\.3\.[0-9]+$')
install -d /opt/scala3
fetch "https://github.com/scala/scala3/releases/download/$scala/scala3-$scala.tar.gz" | tar -xz -C /opt/scala3 --strip-components=1
sbt=$(gh_tag sbt/sbt '^v1\.[0-9]+\.[0-9]+$')
fetch "https://github.com/sbt/sbt/releases/download/$sbt/sbt-${sbt#v}.tgz" | tar -xz -C /opt # -> /opt/sbt
ln -sf /opt/gradle/bin/gradle /opt/kotlinc/bin/kotlin /opt/kotlinc/bin/kotlinc \
	/opt/scala3/bin/scala /opt/scala3/bin/scalac /opt/sbt/bin/sbt /usr/local/bin/

# --- Ruby 4.0 (ruby/ruby-builder prebuilt for 24.04; its prefix is fixed) ---------
ruby=$(gh_tag ruby/ruby-builder '^ruby-4\.0\.[0-9]+$')
rubydir=/opt/hostedtoolcache/Ruby/${ruby#ruby-}/x64
install -d "$rubydir"
fetch "https://github.com/ruby/ruby-builder/releases/download/$ruby/$ruby-ubuntu-24.04-x64.tar.gz" |
	tar -xz -C "$rubydir" --strip-components=1
"$rubydir/bin/ruby" -e 'require "json"; require "openssl"; require "psych"'
for b in "$rubydir"/bin/*; do ln -sf "$b" /usr/local/bin/; done
# user installs gems without sudo, as with a version manager.
chown -R user:user /opt/hostedtoolcache

# --- nvm with Node 22 (the default node stays the /usr/local LTS) -------------------
nvm=$(gh_tag nvm-sh/nvm '^v[0-9.]+$')
install -d -o user -g user /usr/local/nvm
sudo -u user git -c advice.detachedHead=false clone -q --depth 1 -b "$nvm" https://github.com/nvm-sh/nvm.git /usr/local/nvm
cat >/etc/profile.d/nvm.sh <<'EOF'
# nvm (Boat: "nvm also has Node 22"); `nvm use 22` switches this shell.
export NVM_DIR=/usr/local/nvm
[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" --no-use
EOF
sudo -iu user bash -c '. /etc/profile.d/nvm.sh && nvm install --no-progress 22 >/dev/null && nvm cache clear'

# --- quiet SDK telemetry and banners in a sandbox -----------------------------------
cat >>/etc/environment <<'EOF'
DOTNET_CLI_TELEMETRY_OPTOUT=1
DOTNET_NOLOGO=1
EOF
rm -rf "$dl"
