<?php

/**
 * Query Monitor plugin for WordPress
 *
 * @package   query-monitor
 * @link      https://github.com/johnbillion/query-monitor
 * @author    John Blackbourn <john@johnblackbourn.com>
 * @copyright 2009-2018 John Blackbourn
 * @license   GPL v2 or later
 *
 * Plugin Name:  Query Monitor
 * Description:  The Developer Tools panel for WordPress.
 * Version:      3.1.1
 * Plugin URI:   https://github.com/johnbillion/query-monitor
 * Author:       John Blackbourn & contributors
 * Author URI:   https://github.com/johnbillion/query-monitor/graphs/contributors
 * Text Domain:  query-monitor
 * Domain Path:  /languages/
 * Requires PHP: 5.3.6
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 */

/**
 * Determines if Query Monitor should be enabled. We don't
 * want to load it if we don't have to.
 *
 *  - If logged-in user has the `view_query_monitor`
 *    capability, Query Monitor is enabled.
 *  - If a QM_COOKIE is detected, Query Monitor is enabled.
 *
 * Note that we have to set the value for QM_COOKIE here,
 * in order to detect it.
 *
 * Note that we cannot use is_automattician this early, as
 * the user has not yet been set.
 *
 * @param $enable
 *
 * @return bool
 */

require_once( __DIR__ . '/query-monitor/query-monitor.php' );