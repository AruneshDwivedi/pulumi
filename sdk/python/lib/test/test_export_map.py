# Copyright 2016, Pulumi Corporation.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Tests for get_current_export_map, which exposes stack exports for
unit testing assertions.

Part of https://github.com/pulumi/pulumi/issues/6113
"""

from copy import deepcopy

from pulumi.runtime.stack import Stack
from pulumi.runtime import settings
import pulumi


class MyMocks(pulumi.runtime.Mocks):
    def new_resource(self, args: pulumi.runtime.MockResourceArgs):
        return [args.name + "_id", args.inputs]

    def call(self, args: pulumi.runtime.MockCallArgs):
        raise Exception("call")


def test_get_current_export_map():
    settings.reset_options()
    old_settings = deepcopy(settings.SETTINGS)

    def program():
        pulumi.export("fruit", "banana")
        pulumi.export("color", "yellow")

    try:
        Stack(program)

        export_map = pulumi.get_current_export_map()
        assert export_map == {"fruit": "banana", "color": "yellow"}
    finally:
        settings.configure(old_settings)


def test_get_current_export_map_returns_copy():
    settings.reset_options()
    old_settings = deepcopy(settings.SETTINGS)

    def program():
        pulumi.export("key", "value")

    try:
        Stack(program)

        export_map = pulumi.get_current_export_map()
        export_map["key"] = "modified"

        # The original should be unaffected.
        assert pulumi.get_current_export_map() == {"key": "value"}
    finally:
        settings.configure(old_settings)


def test_get_current_export_map_empty():
    settings.reset_options()
    old_settings = deepcopy(settings.SETTINGS)

    def program():
        pass

    try:
        Stack(program)

        export_map = pulumi.get_current_export_map()
        assert export_map == {}
    finally:
        settings.configure(old_settings)
